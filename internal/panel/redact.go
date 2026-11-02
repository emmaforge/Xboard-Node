package panel

import (
	"net/url"
)

// RedactURL returns a copy of rawURL with the panel token query parameter
// replaced by "REDACTED" (a plain word, so it survives query encoding
// verbatim in logs), so token-bearing URLs are safe to write to logs.
//
// B7 note — why the token stays in the query at all: the Xboard panel
// authenticates server nodes through its Server / ServerV2 middleware,
// which validates $request->input('token') against the configured
// server_token. Laravel's input() merges the query string, form fields and
// JSON body, but it never reads request headers, and a GET request has no
// practical body channel. The panel therefore only accepts the token as a
// query parameter (REST GET) or a JSON body field (REST POST); moving it
// to an Authorization header would make the panel reject every request.
// The transport itself is unchanged by this fix — only its log output is
// redacted, and plain-http panel URLs get a separate startup warning in
// the config package.
func RedactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		// An unparsable string cannot be proven token-free, so never
		// print it verbatim.
		return "[unparsable url]"
	}
	q := u.Query()
	if _, ok := q["token"]; !ok {
		return rawURL
	}
	q.Set("token", "REDACTED")
	u.RawQuery = q.Encode()
	return u.String()
}
