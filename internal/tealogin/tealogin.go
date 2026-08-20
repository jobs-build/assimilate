// Package tealogin resolves Gitea/Forgejo access tokens from the config
// files of tea-style CLIs, refreshing expiring OAuth tokens in place. It
// speaks tea's on-disk protocol exactly — the same YAML schema and search
// paths (via adrg/xdg, like tea), the same <config>.lock flock, and the
// same /login/oauth/access_token refresh with tea's public client ID — so
// the file can be shared safely with a concurrently running CLI.
package tealogin

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/adrg/xdg"
	"github.com/gofrs/flock"
	"golang.org/x/oauth2"
	"gopkg.in/yaml.v3"
)

// clientID is tea's OAuth2 client ID, pre-registered in Gitea and Forgejo.
const clientID = "d57cb8c4-630c-4168-8324-ec79935e18d4"

// refreshThreshold matches tea: refresh when this close to expiry.
const refreshThreshold = 5 * time.Minute

// lockTimeout matches tea's config lock timeout.
const lockTimeout = 5 * time.Second

// ErrNoLogin reports that no CLI config holds a login for the instance.
var ErrNoLogin = errors.New("tealogin: no CLI login found")

// configApps are the app dirs probed for a config.yml, in order: a
// Forgejo-branded tea first, then Gitea's tea.
var configApps = [...]string{"forgejo", "tea"}

type login struct {
	Name         string `yaml:"name"`
	URL          string `yaml:"url"`
	Token        string `yaml:"token"`
	AuthMethod   string `yaml:"auth_method"`
	RefreshToken string `yaml:"refresh_token"`
	TokenExpiry  int64  `yaml:"token_expiry"`
	Insecure     bool   `yaml:"insecure"`
}

// Token returns the access token of the CLI login whose URL host matches
// instanceURL, refreshing and persisting it when expired or close to it.
// ErrNoLogin means no config file or no matching login; any other error is
// a found login that could not be used.
func Token(ctx context.Context, instanceURL string) (string, error) {
	path, ok := findConfig()
	if !ok {
		return "", ErrNoLogin
	}
	l, err := matchLogin(path, instanceURL)
	if err != nil {
		return "", err
	}
	if l.AuthMethod == "oauth" {
		return "", fmt.Errorf("tealogin: login %q in %s keeps its token in the CLI's secure credential store; set FORGEJO_TOKEN instead", l.Name, path)
	}
	if !needsRefresh(l) {
		return l.Token, nil
	}
	return refreshUnderLock(ctx, path, instanceURL)
}

func findConfig() (string, bool) {
	for _, app := range configApps {
		if p, err := xdg.SearchConfigFile(app + "/config.yml"); err == nil {
			return p, true
		}
	}
	return "", false
}

// matchLogin parses path and returns the login whose URL host equals
// instanceURL's host; ErrNoLogin when none does.
func matchLogin(path, instanceURL string) (login, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return login{}, fmt.Errorf("tealogin: %w", err)
	}
	var cfg struct {
		Logins []login `yaml:"logins"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return login{}, fmt.Errorf("tealogin: parsing %s: %w", path, err)
	}
	want := hostOf(instanceURL)
	for _, l := range cfg.Logins {
		if want != "" && hostOf(l.URL) == want {
			return l, nil
		}
	}
	return login{}, ErrNoLogin
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

func needsRefresh(l login) bool {
	if l.RefreshToken == "" || l.TokenExpiry == 0 {
		return false
	}
	return time.Now().Add(refreshThreshold).After(time.Unix(l.TokenExpiry, 0))
}

// refreshUnderLock takes tea's config lock, re-reads the config — another
// process may have refreshed meanwhile, and spending a rotated refresh
// token twice would kill the session — then refreshes if still needed and
// persists the rotated tokens.
func refreshUnderLock(ctx context.Context, path, instanceURL string) (string, error) {
	fl := flock.New(path + ".lock")
	lctx, cancel := context.WithTimeout(ctx, lockTimeout)
	defer cancel()
	ok, err := fl.TryLockContext(lctx, 50*time.Millisecond)
	if err != nil {
		return "", fmt.Errorf("tealogin: locking %s: %w", fl.Path(), err)
	}
	if !ok {
		return "", fmt.Errorf("tealogin: timeout locking %s", fl.Path())
	}
	defer fl.Unlock()

	l, err := matchLogin(path, instanceURL)
	if err != nil {
		return "", err
	}
	if !needsRefresh(l) {
		return l.Token, nil
	}

	tok, err := refresh(ctx, l)
	if err != nil {
		return "", err
	}
	if err := persist(path, l.Name, tok); err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

// refresh exchanges the refresh token at the instance's OAuth endpoint.
// The seed token carries only the refresh token: with an access token that
// oauth2 still considers valid it would skip the exchange, but our
// threshold is minutes where oauth2's is seconds.
func refresh(ctx context.Context, l login) (*oauth2.Token, error) {
	if l.Insecure {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		})
	}
	conf := &oauth2.Config{
		ClientID: clientID,
		Endpoint: oauth2.Endpoint{
			TokenURL:  strings.TrimSuffix(l.URL, "/") + "/login/oauth/access_token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
	tok, err := conf.TokenSource(ctx, &oauth2.Token{RefreshToken: l.RefreshToken}).Token()
	if err != nil {
		return nil, fmt.Errorf("tealogin: token refresh for %s failed (re-login with the CLI, or set FORGEJO_TOKEN): %w", l.URL, err)
	}
	return tok, nil
}

// persist rewrites only the matched login's token fields inside the YAML
// document, so every other field, login, and comment — including ones this
// package knows nothing about — survives byte-for-byte.
func persist(path, name string, tok *oauth2.Token) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("tealogin: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("tealogin: parsing %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("tealogin: %s: unexpected document structure", path)
	}
	logins := mapValue(doc.Content[0], "logins")
	if logins == nil || logins.Kind != yaml.SequenceNode {
		return fmt.Errorf("tealogin: %s: no logins list", path)
	}
	for _, entry := range logins.Content {
		if n := mapValue(entry, "name"); n == nil || n.Value != name {
			continue
		}
		setScalar(entry, "token", "!!str", tok.AccessToken)
		if tok.RefreshToken != "" {
			setScalar(entry, "refresh_token", "!!str", tok.RefreshToken)
		}
		if !tok.Expiry.IsZero() {
			setScalar(entry, "token_expiry", "!!int", fmt.Sprint(tok.Expiry.Unix()))
		}
		return writeDoc(path, &doc)
	}
	return fmt.Errorf("tealogin: %s: login %q disappeared", path, name)
}

// mapValue returns the value node of key in mapping m, or nil.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setScalar updates key's scalar value in mapping m, appending the pair
// when absent.
func setScalar(m *yaml.Node, key, tag, value string) {
	if v := mapValue(m, key); v != nil {
		v.Kind, v.Tag, v.Value, v.Style = yaml.ScalarNode, tag, value, 0
		return
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value})
}

// writeDoc marshals the document with tea's indentation and the file's
// existing permissions.
func writeDoc(path string, doc *yaml.Node) error {
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(4)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("tealogin: encoding %s: %w", path, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("tealogin: encoding %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(b.String()), mode); err != nil {
		return fmt.Errorf("tealogin: %w", err)
	}
	return nil
}
