package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cristalhq/jwt/v2"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/mattermost/mattermost/server/public/pluginapi/experimental/telemetry"
	"github.com/mattermost/mattermost/server/public/pluginapi/i18n"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/pkg/errors"
)

const jitsiNameSchemeAsk = "ask"
const jitsiNameSchemeWords = "words"
const jitsiNameSchemeUUID = "uuid"
const jitsiNameSchemeMattermost = "mattermost"
const configChangeEvent = "config_update"

// ringStopEvent tells all clients of a user to stop ringing: the user joined on one of them.
const ringStopEvent = "ring_stop"

type UserConfig struct {
	NamingScheme    string `json:"naming_scheme"`
	Embedded        bool   `json:"embedded"`
	ShowPrejoinPage bool   `json:"show_prejoin_page"`

	// DisableRing turns off the sound played when someone else starts a meeting.
	DisableRing bool `json:"disable_ring,omitempty"`

	// RingOnMeetingStart tells the webapp whether to play that sound: the system setting
	// JitsiRingOnMeetingStart is on, the user has not disabled it and is not listed in
	// JitsiRingExcludedUsers. It is computed on read.
	RingOnMeetingStart bool `json:"ring_on_meeting_start"`

	// RingTimeout is how long, in seconds, the ringtone plays (JitsiRingTimeout). Computed on read.
	RingTimeout int `json:"ring_timeout,omitempty"`
}

type Plugin struct {
	plugin.MattermostPlugin

	client *pluginapi.Client

	telemetryClient telemetry.Client
	tracker         telemetry.Tracker

	// configurationLock synchronizes access to the configuration.
	configurationLock sync.RWMutex

	// configuration is the active plugin configuration. Consult getConfiguration and
	// setConfiguration for usage.
	configuration *configuration

	b *i18n.Bundle

	botID string
}

func (p *Plugin) OnActivate() error {
	config := p.getConfiguration()
	if err := config.IsValid(); err != nil {
		return err
	}

	command, err := p.createJitsiCommand()
	if err != nil {
		return err
	}

	if err = p.API.RegisterCommand(command); err != nil {
		return err
	}

	i18nBundle, err := i18n.InitBundle(p.API, filepath.Join("assets", "i18n"))
	if err != nil {
		return err
	}
	p.b = i18nBundle

	jitsiBot := &model.Bot{
		Username:    "jitsi",
		DisplayName: "Jitsi",
		Description: "A bot account created by the jitsi plugin",
	}
	options := []pluginapi.EnsureBotOption{
		pluginapi.ProfileImagePath("assets/icon.png"),
	}

	p.client = pluginapi.NewClient(p.API, p.Driver)
	botID, ensureBotError := p.client.Bot.EnsureBot(jitsiBot, options...)
	if ensureBotError != nil {
		return errors.Wrap(ensureBotError, "failed to ensure jitsi bot user")
	}

	p.botID = botID

	p.telemetryClient, err = telemetry.NewRudderClient()
	if err != nil {
		p.API.LogWarn("telemetry client not started", "error", err.Error())
	}

	return nil
}

type User struct {
	Avatar string `json:"avatar"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	ID     string `json:"id"`
}

type Context struct {
	User  User   `json:"user"`
	Group string `json:"group"`
}

type EnrichMeetingJwtRequest struct {
	Jwt string `json:"jwt"`
}

// Claims extents cristalhq/jwt standard claims to add jitsi-web-token specific fields
type Claims struct {
	jwt.StandardClaims
	Context Context `json:"context"`
	Room    string  `json:"room,omitempty"`
}

func verifyJwt(secret string, jwtToken string) (*Claims, error) {
	verifier, err := jwt.NewVerifierHS(jwt.HS256, []byte(secret))
	if err != nil {
		mlog.Error("Error generating new HS256 signer", mlog.Err(err))
		return nil, err
	}

	newToken, err := jwt.ParseAndVerifyString(jwtToken, verifier)
	if err != nil {
		mlog.Error("Error parsing or verifiying jwt", mlog.Err(err))
		return nil, err
	}

	var claims Claims
	if err = json.Unmarshal(newToken.RawClaims(), &claims); err != nil {
		mlog.Error("Error unmarshalling claims from jwt", mlog.Err(err))
		return nil, err
	}
	return &claims, nil
}

func signClaims(secret string, claims *Claims) (string, error) {
	signer, err := jwt.NewSignerHS(jwt.HS256, []byte(secret))
	if err != nil {
		mlog.Error("Error generating new HS256 signer", mlog.Err(err))
		return "", errors.New("internal error")
	}
	builder := jwt.NewBuilder(signer)
	token, err := builder.Build(claims)
	if err != nil {
		return "", err
	}
	return string(token.Raw()), nil
}

func (p *Plugin) trackMeeting(_ *model.CommandArgs) {
	// disables tracking if the user is not using the default jitsi url
	isNotDefaultJitsiURL := p.isNotDefaultJitsiURL()
	// enables tracking based on the users configuration
	var event = map[string]interface{}{
		"external-meeting-link-enabled": isNotDefaultJitsiURL,
	}

	_ = p.tracker.TrackEvent("start_meeting", event)
}

func (p *Plugin) isNotDefaultJitsiURL() bool {
	currentURL := p.configuration.GetJitsiURL()
	defaultURL := p.configuration.GetDefaultJitsiURL()
	return currentURL != defaultURL
}

func (p *Plugin) deleteEphemeralPost(userID, postID string) {
	p.API.DeleteEphemeralPost(userID, postID)
}

func (p *Plugin) updateJwtUserInfo(jwtToken string, user *model.User) (string, error) {
	secret := p.getConfiguration().JitsiAppSecret
	sanitizedUser := user.DeepCopy()

	claims, err := verifyJwt(secret, jwtToken)
	if err != nil {
		return "", err
	}

	config := p.API.GetConfig()
	if config.PrivacySettings.ShowFullName == nil || !*config.PrivacySettings.ShowFullName {
		sanitizedUser.FirstName = ""
		sanitizedUser.LastName = ""
	}
	if config.PrivacySettings.ShowEmailAddress == nil || !*config.PrivacySettings.ShowEmailAddress {
		sanitizedUser.Email = ""
	}
	newContext := Context{
		User: User{
			Avatar: fmt.Sprintf("%s/api/v4/users/%s/image?_=%d", *config.ServiceSettings.SiteURL, sanitizedUser.Id, sanitizedUser.LastPictureUpdate),
			Name:   sanitizedUser.GetDisplayName(model.ShowNicknameFullName),
			Email:  sanitizedUser.Email,
			ID:     sanitizedUser.Id,
		},
		Group: claims.Context.Group,
	}

	claims.Context = newContext

	return signClaims(secret, claims)
}

func (p *Plugin) startMeeting(user *model.User, channel *model.Channel, meetingID string, meetingTopic string, _ bool, rootID string) (string, error) {
	l := p.b.GetServerLocalizer()
	if meetingID == "" {
		meetingID = encodeJitsiMeetingID(meetingTopic)
		if meetingID != "" {
			meetingID += "-"
		}
		meetingID += randomString(LETTERS, 20)
	}
	meetingPersonal := false
	defaultMeetingTopic := p.b.LocalizeDefaultMessage(l, &i18n.Message{
		ID:    "jitsi.start_meeting.default_meeting_topic",
		Other: "Jitsi Meeting",
	})

	if len(meetingTopic) < 1 {
		userConfig, err := p.getUserConfig(user.Id)
		if err != nil {
			return "", err
		}

		switch userConfig.NamingScheme {
		case jitsiNameSchemeWords:
			meetingID = generateEnglishTitleName()
		case jitsiNameSchemeUUID:
			meetingID = generateUUIDName()
		case jitsiNameSchemeMattermost:
			if channel.Type == model.ChannelTypeDirect || channel.Type == model.ChannelTypeGroup {
				meetingID = generatePersonalMeetingName(user.Username)
				meetingTopic = p.b.LocalizeWithConfig(l, &i18n.LocalizeConfig{
					DefaultMessage: &i18n.Message{
						ID:    "jitsi.start_meeting.personal_meeting_topic",
						Other: "{{.Name}}'s Personal Meeting",
					},
					TemplateData: map[string]string{"Name": user.GetDisplayName(model.ShowNicknameFullName)},
				})
				meetingPersonal = true
			} else {
				team, teamErr := p.API.GetTeam(channel.TeamId)
				if teamErr != nil {
					return "", teamErr
				}
				meetingTopic = p.b.LocalizeWithConfig(l, &i18n.LocalizeConfig{
					DefaultMessage: &i18n.Message{
						ID:    "jitsi.start_meeting.channel_meeting_topic",
						Other: "{{.ChannelName}} Channel Meeting",
					},
					TemplateData: map[string]string{"ChannelName": channel.DisplayName},
				})
				meetingID = generateTeamChannelName(team.Name, channel.Name)
			}
		default:
			meetingID = generateEnglishTitleName()
		}
	}
	jitsiURL := strings.TrimSpace(p.getConfiguration().GetJitsiURL())
	jitsiURL = strings.TrimRight(jitsiURL, "/")
	meetingURL := jitsiURL + "/" + meetingID
	meetingLink := meetingURL

	var meetingLinkValidUntil = time.Time{}
	JWTMeeting := p.getConfiguration().JitsiJWT
	var jwtToken string

	if JWTMeeting {
		// Error check is done in configuration.IsValid()
		jURL, _ := url.Parse(p.getConfiguration().GetJitsiURL())

		meetingLinkValidUntil = time.Now().Add(time.Duration(p.getConfiguration().JitsiLinkValidTime) * time.Minute)

		claims := Claims{}
		claims.Issuer = p.getConfiguration().JitsiAppID
		claims.Audience = []string{p.getConfiguration().JitsiAppID}
		claims.ExpiresAt = jwt.NewNumericDate(meetingLinkValidUntil)
		claims.Subject = jURL.Hostname()
		claims.Room = meetingID

		var err2 error
		jwtToken, err2 = signClaims(p.getConfiguration().JitsiAppSecret, &claims)
		if err2 != nil {
			return "", err2
		}

		meetingURL = meetingURL + "?jwt=" + jwtToken
	}
	if meetingTopic == "" {
		meetingURL = meetingURL + "#config.callDisplayName=" + url.PathEscape("\""+defaultMeetingTopic+"\"")
	} else {
		meetingURL = meetingURL + "#config.callDisplayName=" + url.PathEscape("\""+meetingTopic+"\"")
	}

	meetingUntil := ""
	if JWTMeeting {
		meetingUntil = p.b.LocalizeWithConfig(l, &i18n.LocalizeConfig{
			DefaultMessage: &i18n.Message{
				ID:    "jitsi.start_meeting.meeting_link_valid_until",
				Other: "Meeting link valid until: {{.Datetime}}",
			},
			TemplateData: map[string]string{"Datetime": meetingLinkValidUntil.Format("Mon Jan 2 15:04:05 -0700 MST 2006")},
		})
	}

	meetingTypeString := p.b.LocalizeWithConfig(l, &i18n.LocalizeConfig{
		DefaultMessage: &i18n.Message{
			ID:    "jitsi.start_meeting.meeting_id",
			Other: "Meeting ID",
		},
	})
	if meetingPersonal {
		meetingTypeString = p.b.LocalizeWithConfig(l, &i18n.LocalizeConfig{
			DefaultMessage: &i18n.Message{
				ID:    "jitsi.start_meeting.personal_meeting_id",
				Other: "Personal Meeting ID (PMI)",
			},
		})
	}

	slackMeetingTopic := meetingTopic
	if slackMeetingTopic == "" {
		slackMeetingTopic = defaultMeetingTopic
	}

	slackAttachment := model.SlackAttachment{
		Fallback: p.b.LocalizeWithConfig(l, &i18n.LocalizeConfig{
			DefaultMessage: &i18n.Message{
				ID: "jitsi.start_meeting.fallback_text",
				Other: `Video Meeting started at [{{.MeetingID}}]({{.MeetingURL}}).

[Join Meeting]({{.MeetingURL}})`,
			},
			TemplateData: map[string]string{
				"MeetingID":  meetingID,
				"MeetingURL": meetingURL,
			},
		}) + "\n\n" + meetingUntil,
		Title: slackMeetingTopic,
		Text: p.b.LocalizeWithConfig(l, &i18n.LocalizeConfig{
			DefaultMessage: &i18n.Message{
				ID: "jitsi.start_meeting.slack_attachment_text",
				Other: `{{.MeetingType}}: [{{.MeetingID}}]({{.MeetingURL}})

[Join Meeting]({{.MeetingURL}})`,
			},
			TemplateData: map[string]string{
				"MeetingType": meetingTypeString,
				"MeetingID":   meetingID,
				"MeetingURL":  meetingURL,
			},
		}) + "\n\n" + meetingUntil,
	}

	post := &model.Post{
		UserId:    user.Id,
		ChannelId: channel.Id,
		Type:      "custom_jitsi",
		Props: map[string]interface{}{
			"attachments":             []*model.SlackAttachment{&slackAttachment},
			"meeting_id":              meetingID,
			"meeting_link":            meetingLink,
			"jwt_meeting":             JWTMeeting,
			"meeting_jwt":             jwtToken,
			"jwt_meeting_valid_until": meetingLinkValidUntil.Unix(),
			"meeting_personal":        meetingPersonal,
			"meeting_topic":           meetingTopic,
			"default_meeting_topic":   defaultMeetingTopic,
		},
		RootId: rootID,
	}
	if channel.Type == model.ChannelTypeOpen || channel.Type == model.ChannelTypePrivate {
		post.AddProp("ring_in_channel", p.ringInChannel(channel.Id))
	}

	if _, err := p.API.CreatePost(post); err != nil {
		return "", err
	}

	return meetingID, nil
}

// ringInChannel tells the webapp whether a meeting in a public or private channel rings like
// a direct message (the channel has at most JitsiRingMaxChannelMembers members) or only chimes.
func (p *Plugin) ringInChannel(channelID string) bool {
	limit := p.getConfiguration().JitsiRingMaxChannelMembers
	if limit <= 0 {
		return true
	}
	stats, appErr := p.API.GetChannelStats(channelID)
	if appErr != nil || stats == nil {
		return false
	}
	return stats.MemberCount <= int64(limit)
}

// MarshalBinary default marshaling to JSON.
func (c Claims) MarshalBinary() (data []byte, err error) {
	return json.Marshal(c)
}

func encodeJitsiMeetingID(meeting string) string {
	reg := regexp.MustCompile("[^a-zA-Z0-9-_]+")
	meeting = strings.ReplaceAll(meeting, " ", "-")
	return reg.ReplaceAllString(meeting, "")
}

func (p *Plugin) askMeetingType(user *model.User, channel *model.Channel, rootID string) error {
	l := p.b.GetUserLocalizer(user.Id)
	apiURL := *p.API.GetConfig().ServiceSettings.SiteURL + "/plugins/jitsi/api/v1/meetings"

	actions := []*model.PostAction{}

	var team *model.Team
	if channel.TeamId != "" {
		team, _ = p.API.GetTeam(channel.TeamId)
	}

	actions = append(actions, &model.PostAction{
		Name: p.b.LocalizeWithConfig(l, &i18n.LocalizeConfig{
			DefaultMessage: &i18n.Message{
				ID:    "jitsi.ask.meeting_name_random_words",
				Other: "Meeting name with random words",
			},
		}),
		Integration: &model.PostActionIntegration{
			URL: apiURL,
			Context: map[string]interface{}{
				"meeting_id":    generateEnglishTitleName(),
				"meeting_topic": "Jitsi Meeting",
				"personal":      true,
			},
		},
	})

	actions = append(actions, &model.PostAction{
		Name: p.b.LocalizeWithConfig(l, &i18n.LocalizeConfig{
			DefaultMessage: &i18n.Message{
				ID:    "jitsi.ask.personal_meeting",
				Other: "Personal meeting",
			},
		}),
		Integration: &model.PostActionIntegration{
			URL: apiURL,
			Context: map[string]interface{}{
				"meeting_id":    generatePersonalMeetingName(user.Username),
				"meeting_topic": fmt.Sprintf("%s's Meeting", user.GetDisplayName(model.ShowNicknameFullName)),
				"personal":      true,
			},
		},
	})

	if channel.Type == model.ChannelTypeOpen || channel.Type == model.ChannelTypePrivate {
		actions = append(actions, &model.PostAction{
			Name: p.b.LocalizeWithConfig(l, &i18n.LocalizeConfig{
				DefaultMessage: &i18n.Message{
					ID:    "jitsi.ask.channel_meeting",
					Other: "Channel meeting",
				},
			}),
			Integration: &model.PostActionIntegration{
				URL: apiURL,
				Context: map[string]interface{}{
					"meeting_id":    generateTeamChannelName(team.Name, channel.Name),
					"meeting_topic": fmt.Sprintf("%s Channel Meeting", channel.DisplayName),
					"personal":      false,
				},
			},
		})
	}

	actions = append(actions, &model.PostAction{
		Name: p.b.LocalizeWithConfig(l, &i18n.LocalizeConfig{
			DefaultMessage: &i18n.Message{
				ID:    "jitsi.ask.uuid_meeting",
				Other: "Meeting name with UUID",
			},
		}),
		Integration: &model.PostActionIntegration{
			URL: apiURL,
			Context: map[string]interface{}{
				"meeting_id":    generateUUIDName(),
				"meeting_topic": "Jitsi Meeting",
				"personal":      false,
			},
		},
	})

	sa := model.SlackAttachment{
		Title: p.b.LocalizeWithConfig(l, &i18n.LocalizeConfig{
			DefaultMessage: &i18n.Message{
				ID:    "jitsi.ask.title",
				Other: "Jitsi Meeting Start",
			},
		}),
		Text: p.b.LocalizeWithConfig(l, &i18n.LocalizeConfig{
			DefaultMessage: &i18n.Message{
				ID:    "jitsi.ask.select_meeting_type",
				Other: "Select type of meeting you want to start",
			},
		}),
		Actions: actions,
	}

	post := &model.Post{
		UserId:    p.botID,
		ChannelId: channel.Id,
		RootId:    rootID,
	}
	post.SetProps(map[string]interface{}{
		"attachments": []*model.SlackAttachment{&sa},
	})
	_ = p.API.SendEphemeralPost(user.Id, post)

	return nil
}

func (p *Plugin) getUserConfig(userID string) (*UserConfig, error) {
	data, appErr := p.API.KVGet("config_" + userID)
	if appErr != nil {
		return nil, appErr
	}

	conf := p.getConfiguration()
	userConfig := UserConfig{
		Embedded:        conf.JitsiEmbedded,
		NamingScheme:    conf.JitsiNamingScheme,
		ShowPrejoinPage: conf.JitsiPrejoinPage,
	}
	if data != nil {
		if err := json.Unmarshal(data, &userConfig); err != nil {
			return nil, err
		}
	}

	userConfig.RingOnMeetingStart = conf.JitsiRingOnMeetingStart && !userConfig.DisableRing && !p.isRingExcluded(userID)
	userConfig.RingTimeout = conf.GetRingTimeout()

	return &userConfig, nil
}

// MessageWillBePosted marks meeting posts from accounts listed in JitsiRingSilentSenders as
// silent, so that a bot or a video conference room that sends meeting links (often in advance)
// does not ring the recipients. The webapp skips posts with the ring_silent prop.
func (p *Plugin) MessageWillBePosted(_ *plugin.Context, post *model.Post) (*model.Post, string) {
	if post == nil || post.Type != "custom_jitsi" || !p.isRingSilentSender(post.UserId) {
		return nil, ""
	}
	post.AddProp("ring_silent", true)
	return post, ""
}

// isRingSilentSender reports whether the user is listed in JitsiRingSilentSenders.
func (p *Plugin) isRingSilentSender(userID string) bool {
	if strings.TrimSpace(p.getConfiguration().JitsiRingSilentSenders) == "" {
		return false
	}
	user, appErr := p.API.GetUser(userID)
	if appErr != nil || user == nil {
		return false
	}
	return p.getConfiguration().IsRingSilentSender(user.Username)
}

// isRingExcluded reports whether the user is listed in JitsiRingExcludedUsers.
func (p *Plugin) isRingExcluded(userID string) bool {
	if strings.TrimSpace(p.getConfiguration().JitsiRingExcludedUsers) == "" {
		return false
	}
	user, appErr := p.API.GetUser(userID)
	if appErr != nil || user == nil {
		return false
	}
	return p.getConfiguration().IsRingExcluded(user.Username)
}

func (p *Plugin) setUserConfig(userID string, config *UserConfig) error {
	stored := *config
	stored.RingTimeout = 0 // computed on read, not stored
	b, err := json.Marshal(stored)
	if err != nil {
		return err
	}

	appErr := p.API.KVSet("config_"+userID, b)
	if appErr != nil {
		return appErr
	}

	p.API.PublishWebSocketEvent(configChangeEvent, nil, &model.WebsocketBroadcast{UserId: userID})
	return nil
}
