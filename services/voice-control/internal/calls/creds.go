package calls

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Realm is the SIP digest realm Kamailio challenges with.
const Realm = "parley"

// SIPCredentials let one user join one call room until ExpiresAt. They follow
// Kamailio's auth_ephemeral format (the TURN REST API scheme):
//
//	username = "<unix expiry>:<user id>.<conversation id>"
//	password = base64(HMAC-SHA1(secret, username))
//
// Kamailio additionally checks that the room in the username matches the
// room being dialled, so credentials can't be reused for another call.
type SIPCredentials struct {
	URI       string    `json:"uri"`   // sip:<conversation id>@<domain>: what to dial
	Proxy     string    `json:"proxy"` // outbound proxy, e.g. sip:203.0.113.5:5060;transport=tcp
	Realm     string    `json:"realm"`
	Username  string    `json:"username"`
	Password  string    `json:"password"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Mint creates credentials for user to join conv.
func Mint(secret []byte, user, conv uuid.UUID, proxy, domain string, ttl time.Duration, now time.Time) SIPCredentials {
	exp := now.Add(ttl).Truncate(time.Second)
	username := fmt.Sprintf("%d:%s.%s", exp.Unix(), user, conv)
	mac := hmac.New(sha1.New, secret)
	mac.Write([]byte(username))
	return SIPCredentials{
		URI:       fmt.Sprintf("sip:%s@%s", conv, domain),
		Proxy:     proxy,
		Realm:     Realm,
		Username:  username,
		Password:  base64.StdEncoding.EncodeToString(mac.Sum(nil)),
		ExpiresAt: exp,
	}
}
