package tealogin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrg/xdg"
	"gopkg.in/yaml.v3"
)

// configHome points XDG at a fresh temp dir (and empties the system dirs)
// so tests never see the developer's real CLI configs.
func configHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_CONFIG_DIRS", filepath.Join(dir, "sysdirs"))
	xdg.Reload()
	t.Cleanup(xdg.Reload)
	return dir
}

func writeConfig(t *testing.T, home, app, content string) string {
	t.Helper()
	path := filepath.Join(home, app, "config.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// patLogin is a config with one plain personal-access-token login.
func patLogin(instanceURL, token string) string {
	return fmt.Sprintf("logins:\n    - name: test\n      url: %s\n      token: %s\n", instanceURL, token)
}

// oauthLogin is a config with one file-based OAuth login.
func oauthLogin(instanceURL, token, refresh string, expiry time.Time) string {
	return fmt.Sprintf(`logins:
    - name: test
      url: %s
      token: %s
      user: dev
      refresh_token: %s
      token_expiry: %d
`, instanceURL, token, refresh, expiry.Unix())
}

// tokenServer fakes the /login/oauth/access_token endpoint and records
// every refresh request's form values.
type tokenServer struct {
	t        *testing.T
	access   string
	refresh  string
	status   int // 0 → 200
	requests []url.Values
}

func (s *tokenServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || r.URL.Path != "/login/oauth/access_token" {
		s.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.t.Error(err)
	}
	s.requests = append(s.requests, r.PostForm)
	w.Header().Set("Content-Type", "application/json")
	if s.status != 0 && s.status != 200 {
		w.WriteHeader(s.status)
		fmt.Fprint(w, `{"error": "invalid_grant"}`)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  s.access,
		"refresh_token": s.refresh,
		"token_type":    "bearer",
		"expires_in":    3600,
	})
}

func newTokenServer(t *testing.T) (*tokenServer, *httptest.Server) {
	s := &tokenServer{t: t, access: "new-access", refresh: "new-refresh"}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

func TestTokenNoConfig(t *testing.T) {
	configHome(t)
	if _, err := Token(context.Background(), "https://git.example.com"); err != ErrNoLogin {
		t.Fatalf("err = %v, want ErrNoLogin", err)
	}
}

func TestTokenFindsTeaConfig(t *testing.T) {
	home := configHome(t)
	writeConfig(t, home, "tea", patLogin("https://git.example.com", "pat-tok"))
	got, err := Token(context.Background(), "https://git.example.com")
	if err != nil || got != "pat-tok" {
		t.Fatalf("got %q, %v; want pat-tok, nil", got, err)
	}
}

func TestTokenPrefersForgejoConfig(t *testing.T) {
	home := configHome(t)
	writeConfig(t, home, "tea", patLogin("https://git.example.com", "tea-tok"))
	writeConfig(t, home, "forgejo", patLogin("https://git.example.com", "forgejo-tok"))
	got, err := Token(context.Background(), "https://git.example.com")
	if err != nil || got != "forgejo-tok" {
		t.Fatalf("got %q, %v; want forgejo-tok, nil", got, err)
	}
}

func TestTokenMatchesHost(t *testing.T) {
	home := configHome(t)
	writeConfig(t, home, "tea", patLogin("https://other.example.com", "other-tok")+
		"    - name: right\n      url: https://git.example.com/\n      token: right-tok\n")

	got, err := Token(context.Background(), "https://git.example.com")
	if err != nil || got != "right-tok" {
		t.Fatalf("got %q, %v; want right-tok, nil", got, err)
	}

	if _, err := Token(context.Background(), "https://absent.example.com"); err != ErrNoLogin {
		t.Fatalf("err = %v, want ErrNoLogin for unmatched host", err)
	}
}

func TestTokenFreshSkipsRefresh(t *testing.T) {
	home := configHome(t)
	s, srv := newTokenServer(t)
	writeConfig(t, home, "tea", oauthLogin(srv.URL, "cur-tok", "cur-refresh", time.Now().Add(time.Hour)))
	got, err := Token(context.Background(), srv.URL)
	if err != nil || got != "cur-tok" {
		t.Fatalf("got %q, %v; want cur-tok, nil", got, err)
	}
	if len(s.requests) != 0 {
		t.Errorf("refresh requests = %d, want 0", len(s.requests))
	}
}

func TestTokenRefreshesExpired(t *testing.T) {
	for _, tc := range []struct {
		name   string
		expiry time.Time
	}{
		{"expired", time.Now().Add(-time.Hour)},
		{"inside threshold", time.Now().Add(4 * time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := configHome(t)
			s, srv := newTokenServer(t)
			path := writeConfig(t, home, "tea",
				"# my tea config\n"+
					oauthLogin(srv.URL, "old-tok", "old-refresh", tc.expiry)+
					"    - name: unrelated\n      url: https://other.example.com\n      token: other-tok\n"+
					"preferences:\n    editor: true\n    unknown_pref: keep-me\n")

			got, err := Token(context.Background(), srv.URL)
			if err != nil || got != "new-access" {
				t.Fatalf("got %q, %v; want new-access, nil", got, err)
			}
			if len(s.requests) != 1 {
				t.Fatalf("refresh requests = %d, want 1", len(s.requests))
			}
			form := s.requests[0]
			if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "old-refresh" {
				t.Errorf("refresh form = %v", form)
			}
			if form.Get("client_id") != clientID {
				t.Errorf("client_id = %q, want %q", form.Get("client_id"), clientID)
			}

			// The rotated tokens are persisted; everything else survives.
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var cfg struct {
				Logins []struct {
					Name         string `yaml:"name"`
					Token        string `yaml:"token"`
					User         string `yaml:"user"`
					RefreshToken string `yaml:"refresh_token"`
					TokenExpiry  int64  `yaml:"token_expiry"`
				} `yaml:"logins"`
				Preferences struct {
					Editor      bool   `yaml:"editor"`
					UnknownPref string `yaml:"unknown_pref"`
				} `yaml:"preferences"`
			}
			if err := yaml.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			if len(cfg.Logins) != 2 {
				t.Fatalf("logins = %d, want 2", len(cfg.Logins))
			}
			l := cfg.Logins[0]
			if l.Token != "new-access" || l.RefreshToken != "new-refresh" || l.User != "dev" {
				t.Errorf("login after refresh = %+v", l)
			}
			if remaining := time.Until(time.Unix(l.TokenExpiry, 0)); remaining < 55*time.Minute || remaining > 65*time.Minute {
				t.Errorf("token_expiry %v from now, want ~1h", remaining)
			}
			if cfg.Logins[1].Token != "other-tok" {
				t.Errorf("unrelated login touched: %+v", cfg.Logins[1])
			}
			if !cfg.Preferences.Editor || cfg.Preferences.UnknownPref != "keep-me" {
				t.Errorf("preferences touched: %+v", cfg.Preferences)
			}
			if !strings.Contains(string(raw), "# my tea config") {
				t.Error("comment dropped on rewrite")
			}

			// A second resolve sees the fresh persisted token: no new request.
			got, err = Token(context.Background(), srv.URL)
			if err != nil || got != "new-access" {
				t.Fatalf("second resolve got %q, %v", got, err)
			}
			if len(s.requests) != 1 {
				t.Errorf("refresh requests after second resolve = %d, want 1", len(s.requests))
			}
		})
	}
}

func TestTokenCredstoreLoginUnsupported(t *testing.T) {
	home := configHome(t)
	writeConfig(t, home, "tea",
		"logins:\n    - name: test\n      url: https://git.example.com\n      auth_method: oauth\n")
	_, err := Token(context.Background(), "https://git.example.com")
	if err == nil || !strings.Contains(err.Error(), "FORGEJO_TOKEN") {
		t.Fatalf("err = %v, want credential-store guidance", err)
	}
}

func TestTokenRefreshFailure(t *testing.T) {
	home := configHome(t)
	s, srv := newTokenServer(t)
	s.status = 400
	writeConfig(t, home, "tea", oauthLogin(srv.URL, "old-tok", "old-refresh", time.Now().Add(-time.Hour)))
	_, err := Token(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "refresh") {
		t.Fatalf("err = %v, want refresh failure", err)
	}
}
