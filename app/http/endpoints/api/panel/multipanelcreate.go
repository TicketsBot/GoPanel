package api

import (
	"context"
	"errors"
	"github.com/TicketsBot/GoPanel/botcontext"
	dbclient "github.com/TicketsBot/GoPanel/database"
	"github.com/TicketsBot/GoPanel/rpc"
	"github.com/TicketsBot/GoPanel/rpc/cache"
	"github.com/TicketsBot/GoPanel/utils"
	"github.com/TicketsBot/common/premium"
	"github.com/TicketsBot/database"
	"github.com/gin-gonic/gin"
	"github.com/rxdn/gdl/objects/channel"
	"github.com/rxdn/gdl/objects/channel/embed"
	"github.com/rxdn/gdl/objects/channel/message"
	"github.com/rxdn/gdl/rest"
	"github.com/rxdn/gdl/rest/request"
	gdlutils "github.com/rxdn/gdl/utils"
	"golang.org/x/sync/errgroup"
)

type multiPanelCreateData struct {
	Title     string                     `json:"title"`
	Content   string                     `json:"content"`
	Colour    int32                      `json:"colour"`
	ChannelId uint64                     `json:"channel_id,string"`
	Panels    gdlutils.Uint64StringSlice `json:"panels"`
}

func MultiPanelCreate(ctx *gin.Context) {
	guildId := ctx.Keys["guildid"].(uint64)

	var data multiPanelCreateData
	if err := ctx.ShouldBindJSON(&data); err != nil {
		ctx.JSON(400, utils.ErrorJson(err))
		return
	}

	// validate body & get sub-panels
	panels, err := data.doValidations(guildId)
	if err != nil {
		ctx.JSON(400, utils.ErrorJson(err))
		return
	}

	// get bot context
	botContext, err := botcontext.ContextForGuild(guildId)
	if err != nil {
		ctx.AbortWithStatusJSON(500, gin.H{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	// get premium status
	premiumTier := rpc.PremiumClient.GetTierByGuildId(guildId, true, botContext.Token, botContext.RateLimiter)

	messageId, err := data.sendEmbed(&botContext, premiumTier > premium.None)
	if err != nil {
		var unwrapped request.RestError
		if errors.As(err, &unwrapped); unwrapped.ErrorCode == 403 {
			ctx.JSON(500, utils.ErrorJson(errors.New("I do not have permission to send messages in the provided channel")))
		} else {
			ctx.JSON(500, utils.ErrorJson(err))
		}

		return
	}

	if err := data.addReactions(&botContext, data.ChannelId, messageId, panels); err != nil {
		var unwrapped request.RestError
		if errors.As(err, &unwrapped); unwrapped.ErrorCode == 403{
			ctx.JSON(500, utils.ErrorJson(errors.New("I do not have permission to add reactions in the provided channel")))
		} else {
			ctx.JSON(500, utils.ErrorJson(err))
		}

		return
	}

	multiPanel := database.MultiPanel{
		MessageId: messageId,
		ChannelId: data.ChannelId,
		GuildId:   guildId,
		Title:     data.Title,
		Content:   data.Content,
		Colour:    int(data.Colour),
	}

	multiPanel.Id, err = dbclient.Client.MultiPanels.Create(multiPanel)
	if err != nil {
		ctx.JSON(500, utils.ErrorJson(err))
		return
	}

	group, _ := errgroup.WithContext(context.Background())
	for _, panel := range panels {
		panel := panel

		group.Go(func() error {
			return dbclient.Client.MultiPanelTargets.Insert(multiPanel.Id, panel.MessageId)
		})
	}

	if err := group.Wait(); err != nil {
		ctx.JSON(500, utils.ErrorJson(err))
		return
	}

	ctx.JSON(200, gin.H{
		"success": true,
		"data": multiPanel,
	})
}

func (d *multiPanelCreateData) doValidations(guildId uint64) (panels []database.Panel, err error) {
	group, _ := errgroup.WithContext(context.Background())

	group.Go(d.validateTitle)
	group.Go(d.validateContent)
	group.Go(d.validateChannel(guildId))
	group.Go(func() (e error) {
		panels, e = d.validatePanels(guildId)
		return
	})

	err = group.Wait()
	return
}

func (d *multiPanelCreateData) validateTitle() (err error) {
	if len(d.Title) > 255 || len(d.Title) < 1 {
		err = errors.New("embed title must be between 1 and 255 characters")
	}
	return
}

func (d *multiPanelCreateData) validateContent() (err error) {
	if len(d.Content) > 1024 || len(d.Title) < 1 {
		err = errors.New("embed content must be between 1 and 1024 characters")
	}
	return
}

func (d *multiPanelCreateData) validateChannel(guildId uint64) func() error {
	return func() (err error) {
		channels := cache.Instance.GetGuildChannels(guildId)

		var valid bool
		for _, ch := range channels {
			if ch.Id == d.ChannelId && ch.Type == channel.ChannelTypeGuildText {
				valid = true
				break
			}
		}

		if !valid {
			err = errors.New("channel does not exist")
		}

		return
	}
}

func (d *multiPanelCreateData) validatePanels(guildId uint64) (panels []database.Panel, err error) {
	if len(d.Panels) < 2 {
		err = errors.New("a multi-panel must contain at least 2 sub-panels")
		return
	}

	if len(d.Panels) > 15 {
		err = errors.New("multi-panels cannot contain more than 15 sub-panels")
		return
	}

	existingPanels, err := dbclient.Client.Panel.GetByGuild(guildId)
	if err != nil {
		return nil, err
	}

	for _, panelId := range d.Panels {
		var valid bool
		// find panel struct
		for _, panel := range existingPanels {
			if panel.MessageId == panelId {
				// check there isn't a panel with the same reaction emote
				for _, previous := range panels {
					if previous.ReactionEmote == panel.ReactionEmote {
						return nil, errors.New("2 sub-panels cannot have the same reaction emotes")
					}
				}

				valid = true
				panels = append(panels, panel)
			}
		}

		if !valid {
			return nil, errors.New("invalid panel ID")
		}
	}

	return
}

func (d *multiPanelCreateData) sendEmbed(ctx *botcontext.BotContext, isPremium bool) (messageId uint64, err error) {
	e := embed.NewEmbed().
		SetTitle(d.Title).
		SetDescription(d.Content).
		SetColor(int(d.Colour))

	if !isPremium {
		// TODO: Don't harcode
		e.SetFooter("Powered by ticketsbot.net", "https://cdn.discordapp.com/avatars/508391840525975553/ac2647ffd4025009e2aa852f719a8027.png?size=256")
	}

	var msg message.Message
	msg, err = rest.CreateMessage(ctx.Token, ctx.RateLimiter, d.ChannelId, rest.CreateMessageData{Embed: e})
	if err != nil {
		return
	}

	messageId = msg.Id
	return
}


func (d *multiPanelCreateData) addReactions(ctx *botcontext.BotContext, channelId, messageId uint64, panels []database.Panel) (err error) {
	for _, panel := range panels {
		if err = rest.CreateReaction(ctx.Token, ctx.RateLimiter, channelId, messageId, panel.ReactionEmote); err != nil {
			return err
		}
	}

	return
}