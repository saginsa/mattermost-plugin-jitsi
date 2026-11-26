package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristalhq/jwt/v2"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/mattermost/mattermost/server/public/pluginapi/i18n"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestVerifyJwt(t *testing.T) {
	testToken := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.mpHl842O7xEZjgQ8CyX8xYLDoEORGVMnAxULkW-u8Ek"
	t.Run("bad secret should fail", func(t *testing.T) {
		claims, err := verifyJwt("bad-secret", testToken)
		require.NotNil(t, err)
		require.Nil(t, claims)
	})
	t.Run("right secret should work", func(t *testing.T) {
		claims, err := verifyJwt("test-secret", testToken)
		require.Nil(t, err)
		require.Equal(t, "1234567890", claims.Subject)
		issuedAt, err := claims.IssuedAt.MarshalJSON()
		require.Nil(t, err)
		require.Equal(t, "1516239022", string(issuedAt))
	})
}

func TestSignClaims(t *testing.T) {
	claims := Claims{}
	claims.Issuer = "test"
	claims.Audience = []string{"test-app-id"}
	claims.ExpiresAt = jwt.NewNumericDate(time.Time{})
	claims.Subject = "test"
	claims.Room = "test"

	token, err := signClaims("test-secret", &claims)
	require.Nil(t, err)

	newClaims, err := verifyJwt("test-secret", token)
	require.Nil(t, err)
	require.Equal(t, &claims, newClaims)
}

func TestStartMeeting(t *testing.T) {
	p := Plugin{
		configuration: &configuration{
			JitsiURL: "http://test",
		},
	}
	apiMock := plugintest.API{}
	defer apiMock.AssertExpectations(t)
	apiMock.On("GetBundlePath").Return("..", nil)
	config := model.Config{}
	config.SetDefaults()
	apiMock.On("GetConfig").Return(&config, nil)

	p.SetAPI(&apiMock)

	i18nBundle, err := i18n.InitBundle(p.API, filepath.Join("assets", "i18n"))
	require.Nil(t, err)
	p.b = i18nBundle

	testUser := model.User{Id: "test-id", Username: "test-username", FirstName: "test-first-name", LastName: "test-last-name", Nickname: "test-nickname"}
	testChannel := model.Channel{Id: "test-id", Type: model.ChannelTypeDirect, Name: "test-name", DisplayName: "test-display-name"}

	t.Run("start meeting without topic or id", func(t *testing.T) {
		apiMock.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
			return strings.HasPrefix(post.Props["meeting_link"].(string), "http://test/")
		})).Return(&model.Post{}, nil)
		b, _ := json.Marshal(UserConfig{Embedded: false, NamingScheme: "mattermost"})
		apiMock.On("KVGet", "config_test-id", mock.Anything).Return(b, nil)

		meetingID, err := p.startMeeting(&testUser, &testChannel, "", "", false, "")
		require.Nil(t, err)
		require.Regexp(t, "^test-username-", meetingID)
	})

	t.Run("start meeting with topic and without id", func(t *testing.T) {
		apiMock.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
			return strings.HasPrefix(post.Props["meeting_link"].(string), "http://test/")
		})).Return(&model.Post{}, nil)
		b, _ := json.Marshal(UserConfig{Embedded: false, NamingScheme: "mattermost"})
		apiMock.On("KVGet", "config_test-id", mock.Anything).Return(b, nil)

		meetingID, err := p.startMeeting(&testUser, &testChannel, "", "Test topic", false, "")
		require.Nil(t, err)
		require.Regexp(t, "^Test-topic-", meetingID)
	})

	t.Run("start meeting without topic and with id", func(t *testing.T) {
		apiMock.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
			return strings.HasPrefix(post.Props["meeting_link"].(string), "http://test/")
		})).Return(&model.Post{}, nil)
		b, _ := json.Marshal(UserConfig{Embedded: false, NamingScheme: "mattermost"})
		apiMock.On("KVGet", "config_test-id", mock.Anything).Return(b, nil)

		meetingID, err := p.startMeeting(&testUser, &testChannel, "test-id", "", false, "")
		require.Nil(t, err)
		require.Regexp(t, "^test-username-", meetingID)
	})

	t.Run("start meeting with topic and id", func(t *testing.T) {
		testUser := model.User{Id: "test-id", Username: "test-username", FirstName: "test-first-name", LastName: "test-last-name", Nickname: "test-nickname"}
		testChannel := model.Channel{Id: "test-id", Type: model.ChannelTypeOpen, TeamId: "test-team-id", Name: "test-name", DisplayName: "test-display-name"}

		apiMock.On("CreatePost", mock.MatchedBy(func(post *model.Post) bool {
			return strings.HasPrefix(post.Props["meeting_link"].(string), "http://test/")
		})).Return(&model.Post{}, nil)
		b, _ := json.Marshal(UserConfig{Embedded: false, NamingScheme: "mattermost"})
		apiMock.On("KVGet", "config_test-id", mock.Anything).Return(b, nil)

		meetingID, err := p.startMeeting(&testUser, &testChannel, "test-id", "Test topic", false, "")
		require.Nil(t, err)
		require.Equal(t, "test-id", meetingID)
	})
}

func TestGetUserConfigRingOnMeetingStart(t *testing.T) {
	for _, ring := range []bool{true, false} {
		p := Plugin{configuration: &configuration{JitsiRingOnMeetingStart: ring}}
		apiMock := plugintest.API{}
		p.SetAPI(&apiMock)

		t.Run("without saved user settings", func(t *testing.T) {
			apiMock.On("KVGet", "config_new-user").Return(nil, nil)
			userConfig, err := p.getUserConfig("new-user")
			require.NoError(t, err)
			require.Equal(t, ring, userConfig.RingOnMeetingStart)
		})

		t.Run("with saved user settings, the stored computed value is ignored", func(t *testing.T) {
			b, _ := json.Marshal(UserConfig{NamingScheme: "uuid", RingOnMeetingStart: !ring})
			apiMock.On("KVGet", "config_saved-user").Return(b, nil)
			userConfig, err := p.getUserConfig("saved-user")
			require.NoError(t, err)
			require.Equal(t, ring, userConfig.RingOnMeetingStart)
			require.Equal(t, "uuid", userConfig.NamingScheme)
		})

		t.Run("the user turned the ring off", func(t *testing.T) {
			b, _ := json.Marshal(UserConfig{NamingScheme: "uuid", DisableRing: true})
			apiMock.On("KVGet", "config_quiet-user").Return(b, nil)
			userConfig, err := p.getUserConfig("quiet-user")
			require.NoError(t, err)
			require.False(t, userConfig.RingOnMeetingStart)
		})
	}
}

func TestGetUserConfigRingTimeout(t *testing.T) {
	for _, c := range []struct{ set, want int }{{0, 60}, {-1, 60}, {1, 5}, {90, 90}, {5000, 600}} {
		p := Plugin{configuration: &configuration{JitsiRingOnMeetingStart: true, JitsiRingTimeout: c.set}}
		apiMock := plugintest.API{}
		p.SetAPI(&apiMock)
		apiMock.On("KVGet", "config_user").Return(nil, nil)
		userConfig, err := p.getUserConfig("user")
		require.NoError(t, err)
		require.Equal(t, c.want, userConfig.RingTimeout, "JitsiRingTimeout=%d", c.set)
	}
}

func TestGetUserConfigRingExcludedUsers(t *testing.T) {
	p := Plugin{configuration: &configuration{JitsiRingOnMeetingStart: true, JitsiRingExcludedUsers: " vks-room1, @VKS-Room2;vks3\n"}}
	apiMock := plugintest.API{}
	p.SetAPI(&apiMock)
	apiMock.On("KVGet", mock.Anything).Return(nil, nil)
	apiMock.On("GetUser", "room").Return(&model.User{Id: "room", Username: "vks-room2"}, nil)
	apiMock.On("GetUser", "person").Return(&model.User{Id: "person", Username: "ivanov_ii"}, nil)

	userConfig, err := p.getUserConfig("room")
	require.NoError(t, err)
	require.False(t, userConfig.RingOnMeetingStart)

	userConfig, err = p.getUserConfig("person")
	require.NoError(t, err)
	require.True(t, userConfig.RingOnMeetingStart)
}

func TestMessageWillBePostedSilencesListedSenders(t *testing.T) {
	p := Plugin{configuration: &configuration{JitsiRingOnMeetingStart: true, JitsiRingExcludedUsers: "ivanov_ii", JitsiRingSilentSenders: "meeting-bot"}}
	apiMock := plugintest.API{}
	p.SetAPI(&apiMock)
	apiMock.On("GetUser", "bot").Return(&model.User{Id: "bot", Username: "meeting-bot"}, nil)
	apiMock.On("GetUser", "person").Return(&model.User{Id: "person", Username: "ivanov_ii"}, nil)

	post, reason := p.MessageWillBePosted(nil, &model.Post{UserId: "bot", Type: "custom_jitsi"})
	require.Empty(t, reason)
	require.NotNil(t, post)
	require.Equal(t, true, post.GetProp("ring_silent"))

	post, _ = p.MessageWillBePosted(nil, &model.Post{UserId: "person", Type: "custom_jitsi"})
	require.Nil(t, post)

	// Other posts are not looked at.
	post, _ = p.MessageWillBePosted(nil, &model.Post{UserId: "bot", Message: "hello"})
	require.Nil(t, post)
	apiMock.AssertNumberOfCalls(t, "GetUser", 2)
}

func TestHandleRingStop(t *testing.T) {
	p := Plugin{configuration: &configuration{}}
	apiMock := plugintest.API{}
	p.SetAPI(&apiMock)
	var nilMap map[string]interface{}
	apiMock.On("PublishWebSocketEvent", ringStopEvent, nilMap, &model.WebsocketBroadcast{UserId: "user"}).Return()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/ring/stop", nil)
	r.Header.Set("Mattermost-User-Id", "user")
	p.ServeHTTP(nil, w, r)
	require.Equal(t, http.StatusOK, w.Code)
	apiMock.AssertNumberOfCalls(t, "PublishWebSocketEvent", 1)

	w = httptest.NewRecorder()
	p.ServeHTTP(nil, w, httptest.NewRequest(http.MethodPost, "/api/v1/ring/stop", nil))
	require.Equal(t, http.StatusUnauthorized, w.Code)
	apiMock.AssertNumberOfCalls(t, "PublishWebSocketEvent", 1)
}

func TestRingInChannel(t *testing.T) {
	for _, c := range []struct {
		limit   int
		members int64
		want    bool
	}{{0, 500, true}, {50, 50, true}, {50, 51, false}, {-1, 500, true}} {
		p := Plugin{configuration: &configuration{JitsiRingMaxChannelMembers: c.limit}}
		apiMock := plugintest.API{}
		p.SetAPI(&apiMock)
		apiMock.On("GetChannelStats", "ch").Return(&model.ChannelStats{ChannelId: "ch", MemberCount: c.members}, nil)
		require.Equal(t, c.want, p.ringInChannel("ch"), "limit %d, members %d", c.limit, c.members)
	}

	p := Plugin{configuration: &configuration{JitsiRingMaxChannelMembers: 50}}
	apiMock := plugintest.API{}
	p.SetAPI(&apiMock)
	apiMock.On("GetChannelStats", "ch").Return(nil, &model.AppError{Message: "boom"})
	require.False(t, p.ringInChannel("ch"))
}
