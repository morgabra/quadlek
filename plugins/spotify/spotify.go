//go:generate protoc --go_out=. spotify.proto

package spotify

import (
	"context"
	"errors"
	"fmt"

	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/boltdb/bolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/golang/protobuf/proto"
	"github.com/jirwin/quadlek/quadlek"
	uuid "github.com/satori/go.uuid"
	"github.com/zmb3/spotify"
	"golang.org/x/oauth2"
)

func (at *AuthToken) GetOauthToken() *oauth2.Token {
	return &oauth2.Token{
		AccessToken:  at.Token.AccessToken,
		TokenType:    at.Token.TokenType,
		RefreshToken: at.Token.RefreshToken,
		Expiry:       time.Unix(at.Token.ExpiresAt, 0),
	}
}

func startAuthFlow(stateId string) string {
	auth := spotify.NewAuthenticator(fmt.Sprintf("%s/%s", quadlek.WebhookRoot, "spotifyAuthorize"), spotify.ScopePlaylistModifyPublic, spotify.ScopePlaylistModifyPrivate, spotify.ScopeUserReadCurrentlyPlaying)

	url := auth.AuthURL(stateId)

	return url
}

func nowPlaying(ctx context.Context, cmdChannel <-chan *quadlek.CommandMsg) {
	for {
		select {
		case cmdMsg := <-cmdChannel:
			err := cmdMsg.Store.UpdateRaw(func(bkt *bolt.Bucket) error {
				authToken := &AuthToken{}
				authTokenBytes := bkt.Get([]byte("authtoken-" + cmdMsg.Command.UserId))
				err := proto.Unmarshal(authTokenBytes, authToken)
				if err != nil {
					log.WithFields(log.Fields{
						"err": err,
					}).Error("error unmarshalling auth token")
					return err
				}

				if authToken.Token == nil {
					stateId := uuid.NewV4().String()
					authUrl := startAuthFlow(stateId)

					authState := &AuthState{
						Id:          stateId,
						UserId:      cmdMsg.Command.UserId,
						ResponseUrl: cmdMsg.Command.ResponseUrl,
						ExpireTime:  time.Now().UnixNano() + int64(time.Minute*15),
					}

					authStateBytes, err := proto.Marshal(authState)
					if err != nil {
						log.WithFields(log.Fields{
							"err": err,
						}).Error("error marshalling auth state")
						return err
					}

					err = bkt.Put([]byte("authstate-"+stateId), authStateBytes)
					if err != nil {
						cmdMsg.Command.Reply() <- &quadlek.CommandResp{
							Text: "There was an error authenticating to Spotify.",
						}
						return err
					}

					cmdMsg.Command.Reply() <- &quadlek.CommandResp{
						Text: fmt.Sprintf("You need to be authenticate to Spotify to continue. Please visit %s to do this.", authUrl),
					}
					return nil
				}

				auth := spotify.NewAuthenticator(fmt.Sprintf("%s/%s", quadlek.WebhookRoot, "spotifyAuthorize"), spotify.ScopePlaylistModifyPublic, spotify.ScopePlaylistModifyPrivate, spotify.ScopeUserReadCurrentlyPlaying)
				client := auth.NewClient(authToken.GetOauthToken())

				playing, err := client.PlayerCurrentlyPlaying()
				if err != nil {
					cmdMsg.Command.Reply() <- &quadlek.CommandResp{
						Text: "Unable to get currently playing.",
					}
					log.WithFields(log.Fields{
						"err": err,
					}).Error("error getting currently playing.")
					return err
				}

				cmdMsg.Command.Reply() <- &quadlek.CommandResp{
					Text:      fmt.Sprintf("<@%s> is listening to %s", cmdMsg.Command.UserId, playing.Item.URI),
					InChannel: true,
				}

				return nil
			})
			if err != nil {
				cmdMsg.Bot.RespondToSlashCommand(cmdMsg.Command.ResponseUrl, &quadlek.CommandResp{
					Text: "Unable to run now playing.",
				})
			}

		case <-ctx.Done():
			log.Info("Exiting NowPlayingCommand.")
			return
		}
	}
}

func spotifyAuthorizeWebhook(ctx context.Context, whChannel <-chan *quadlek.WebhookMsg) {
	for {
		select {
		case whMsg := <-whChannel:
			query := whMsg.Request.URL.Query()
			stateId, ok := query["state"]
			whMsg.Request.Body.Close()
			if !ok {
				log.WithFields(log.Fields{
					"url": whMsg.Request.URL.String(),
				}).Error("invalid callback url")
				continue
			}

			spew.Dump(whMsg.Request.URL)

			err := whMsg.Store.UpdateRaw(func(bkt *bolt.Bucket) error {
				authStateBytes := bkt.Get([]byte("authstate-" + stateId[0]))
				authState := &AuthState{}
				err := proto.Unmarshal(authStateBytes, authState)
				if err != nil {
					whMsg.Bot.RespondToSlashCommand(authState.ResponseUrl, &quadlek.CommandResp{
						Text: "Sorry! There was an error logging you into Spotify.",
					})
					return err
				}

				now := time.Now().UnixNano()
				if authState.ExpireTime < now {
					bkt.Delete([]byte("authstate-" + stateId[0]))
					whMsg.Bot.RespondToSlashCommand(authState.ResponseUrl, &quadlek.CommandResp{
						Text: "Sorry! There was an error logging you into Spotify.",
					})
					return errors.New("Received expired auth request")
				}

				auth := spotify.NewAuthenticator(fmt.Sprintf("%s/%s", quadlek.WebhookRoot, "spotifyAuthorize"))
				token, err := auth.Token(stateId[0], whMsg.Request)
				if err != nil {
					whMsg.Bot.RespondToSlashCommand(authState.ResponseUrl, &quadlek.CommandResp{
						Text: "Sorry! There was an error logging you into Spotify.",
					})
					return err
				}

				authToken := &AuthToken{
					Token: &Token{
						AccessToken:  token.AccessToken,
						TokenType:    token.TokenType,
						RefreshToken: token.RefreshToken,
						ExpiresAt:    token.Expiry.UnixNano(),
					},
				}

				spew.Dump(authToken.Token.ExpiresAt)

				tokenBytes, err := proto.Marshal(authToken)
				err = bkt.Put([]byte("authtoken-"+authState.UserId), tokenBytes)
				if err != nil {
					whMsg.Bot.RespondToSlashCommand(authState.ResponseUrl, &quadlek.CommandResp{
						Text: "Sorry! There was an error logging you into Spotify.",
					})
					log.Error("error storing auth token.")
					return err
				}

				whMsg.Bot.RespondToSlashCommand(authState.ResponseUrl, &quadlek.CommandResp{
					Text: "Successfully logged into Spotify. Try your command again please.",
				})

				return nil
			})
			if err != nil {
				log.WithFields(log.Fields{
					"err": err,
				}).Error("error authenticating to spotify")
				continue
			}

		case <-ctx.Done():
			log.Info("Exiting spotify authorize command")
			return
		}
	}
}

func Register() quadlek.Plugin {
	return quadlek.MakePlugin(
		"spotify",
		[]quadlek.Command{
			quadlek.MakeCommand("nowplaying", nowPlaying),
		},
		nil,
		[]quadlek.Webhook{
			quadlek.MakeWebhook("spotifyAuthorize", spotifyAuthorizeWebhook),
		},
		nil,
	)
}