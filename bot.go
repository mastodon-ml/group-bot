package main

import (
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/mattn/go-mastodon"
)

var (
	c = mastodon.NewClient(&mastodon.Config{
		Server:       Conf.Server,
		ClientID:     Conf.ClientID,
		ClientSecret: Conf.ClientSecret,
		AccessToken:  Conf.AccessToken,
	})

	ctx = context.Background()

	my_account, _ = c.GetAccountCurrentUser(ctx)
	boostsenabled = true
)

type APobject struct {
	InReplyTo *string `json:"inReplyTo"`
}

// CheckAPReply return is reply bool of status
// Note: Not working with servers when they required Authorized fetch
// Note: By default false
func CheckAPReply(tooturl string) bool {
	var apobj APobject
	client := &http.Client{}
	req, err := http.NewRequest(http.MethodGet, tooturl, nil)
	if err != nil {
		ErrorLogger.Println("Failed http request status AP")
		return false
	}
	req.Header.Set("Accept", "application/activity+json")
	resp, err := client.Do(req)
	if err != nil {
		ErrorLogger.Printf("Server was not return AP object: %s", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		ErrorLogger.Printf("AP get: Server was return %d http code %s", resp.StatusCode, resp.Body)
		return false
	}

	var reader io.ReadCloser
	switch resp.Header.Get("Content-Encoding") {
	case "gzip":
		reader, err = gzip.NewReader(resp.Body)
		defer reader.Close()
	default:
		reader = resp.Body
	}

	err = json.NewDecoder(reader).Decode(&apobj)
	if err != nil {
		ErrorLogger.Println("Failed decoding AP object")
		return false
	}
	if apobj.InReplyTo != nil {
		WarnLogger.Printf("AP object of status detected reply: %s", *apobj.InReplyTo)
		return true
	}
	return false
}

func RunBot() {
	events, err := c.StreamingUser(ctx)
	if err != nil {
		ErrorLogger.Println("Streaming")
	}

	// Run bot
	for {
		notifEvent, ok := (<-events).(*mastodon.NotificationEvent)
		if !ok {
			continue
		}

		notif := notifEvent.Notification

		// New follower
		if notif.Type == "follow" {
			acct := notif.Account.Acct

			if !exist_in_database(acct) { // Add to db and post welcome message
				InfoLogger.Printf("%s followed", acct)

				add_to_db(acct)
				InfoLogger.Printf("%s added to database", acct)

				message := fmt.Sprintf("%s @%s", Conf.WelcomeMessage, acct)
				_, err := postToot(message, "public")
				if err != nil {
					ErrorLogger.Println("Post welcome message")
				}
				InfoLogger.Printf("%s was welcomed", acct)
			}
		}

		// Read message
		if notif.Type == "mention" {
			var account_id = []string{string(notif.Status.Account.ID)}
			acct := notif.Status.Account.Acct
			content := notif.Status.Content
			tooturl := notif.Status.URL

			// Fetch relationship
			relationship, err := c.GetAccountRelationships(ctx, account_id)
			if err != nil {
				ErrorLogger.Println("Fetch relationship")
			}

			// Follow check
			if relationship[0].FollowedBy {
				if notif.Status.Visibility == "public" { // Reblog toot
					if boostsenabled == false {
						continue
					}
					var APreply bool
					APreply = false
					if notif.Status.InReplyToID == nil {
						// Replies protection by get ActivityPub object
						// (if breaking threads)
						APreply = CheckAPReply(tooturl)
					}
					if notif.Status.InReplyToID == nil && APreply == false { // Not boost replies
						// Duplicate protection
						content_hash := sha512.New()
						content_hash.Write([]byte(content))
						hash := fmt.Sprintf("%x", content_hash.Sum(nil))

						if !check_msg_hash(hash) {
							save_msg_hash(hash)
							InfoLogger.Printf("Hash of %s added to database", tooturl)
						} else {
							WarnLogger.Printf("%s is a duplicate and not boosted", tooturl)
						}

						// Add to db if needed
						if !exist_in_database(acct) {
							add_to_db(acct)
							InfoLogger.Printf("%s added to database", acct)
						}

						// Message order
						if check_order(acct) < Conf.Order_limit {
							if check_ticket(acct) > 0 { // Message limit
								take_ticket(acct)
								InfoLogger.Printf("Ticket of %s was taken", acct)

								count_order(acct)
								InfoLogger.Printf("Order of %s was counted", acct)

								c.Reblog(ctx, notif.Status.ID)
								InfoLogger.Printf("Toot %s of %s was rebloged", tooturl, acct)
							} else {
								WarnLogger.Printf("%s haven't tickets", acct)
							}
						} else {
							WarnLogger.Printf("%s order limit", acct)
						}
					} else {
						WarnLogger.Printf("%s is reply and not boosted", tooturl)
					}
				} else if notif.Status.Visibility == "direct" { // Admin commands
					for y := range Conf.Admins {
						if acct == Conf.Admins[y] {
							recmd := regexp.MustCompile(`<[^>]+>`)
							command := recmd.ReplaceAllString(content, "")
							args := strings.Split(command, " ")

							if len(args) == 3 {
								mID := mastodon.ID((args[2]))

								switch args[1] {
								case "boost":
									c.Reblog(ctx, mID)
									WarnLogger.Printf("%s was rebloged", mID)
								case "unboost":
									c.Unreblog(ctx, mID)
									WarnLogger.Printf("%s was unrebloged", mID)
								case "delete":
									c.DeleteStatus(ctx, mID)
									WarnLogger.Printf("%s was deleted", mID)
								case "disable":
									boostsenabled = false
									WarnLogger.Printf("Reblogs disabled by admin")
								case "enable":
									boostsenabled = true
									WarnLogger.Printf("Reblogs enabled by admin")
								default:
									WarnLogger.Printf("%s entered wrong command", acct)
								}
							} else if len(args) == 2 {
								switch args[1] {
								case "disable":
									boostsenabled = false
									WarnLogger.Printf("Reblogs disabled by admin")
								case "enable":
									boostsenabled = true
									WarnLogger.Printf("Reblogs enabled by admin")
								default:
									WarnLogger.Printf("%s entered wrong command", acct)
								}
							} else {
								WarnLogger.Printf("%s entered wrong command", acct)
							}
						}
					}
				} else {
					WarnLogger.Printf("%s is not public toot and not boosted", tooturl)
				}
			} else { // Notify user
				if got_notice(acct) == 0 {
					if !exist_in_database(acct) {
						add_to_db(acct)
						InfoLogger.Printf("%s added to database", acct)
					}
					if notif.Status.InReplyToID == nil { // Prevent spam in DM if status is reply
						message := fmt.Sprintf("@%s %s", acct, Conf.NotFollowedMessage)
						_, err := postToot(message, "direct")
						if err != nil {
							ErrorLogger.Printf("Notify %s", acct)
						}
						InfoLogger.Printf("%s has been notified", acct)
						mark_notice(acct)
						if got_notice(acct) == 0 {
							InfoLogger.Printf("Dooble notice marked")
							mark_notice(acct)
						}
						InfoLogger.Printf("%s marked notification in database", acct)
					} else {
						InfoLogger.Printf("%s their status is reply, not notified", acct)
					}

				}
			}
		}
	}
}
