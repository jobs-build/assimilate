// Package argocd is a minimal ArgoCD REST client: refresh and sync the
// configured applications after a rollout merge.
package argocd

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jobs-build/assimilate/spec"
)

// maxExcerpt caps the response-body excerpt included in non-2xx errors.
const maxExcerpt = 300

// refreshTimeout bounds one refresh request. GET ?refresh=normal blocks
// server-side until ArgoCD finishes re-generating the application's
// manifests, which routinely takes minutes for large apps — hence the
// generous bound.
const refreshTimeout = 5 * time.Minute

// syncTimeout bounds one sync request; POST /sync only queues the operation.
const syncTimeout = 2 * time.Minute

// Rollout refreshes then syncs each app in order:
//
//	GET  <server>/api/v1/applications/<name>?refresh=normal[&appNamespace=…]
//	POST <server>/api/v1/applications/<name>/sync
//
// with Authorization: Bearer <token>; insecure skips TLS verification.
// Per-app outcomes go to log; the returned error aggregates the failures
// (one app failing does not stop the others).
func Rollout(ctx context.Context, apps []spec.ArgoApp, token string, insecure bool, log func(string)) error {
	client := newClient(insecure)
	var errs []error
	for _, app := range apps {
		if err := refresh(ctx, client, app, token); err != nil {
			err = fmt.Errorf("argocd: refresh %s: %w", app.Name, err)
			log(err.Error())
			errs = append(errs, err)
			continue // no point syncing an app we could not even refresh
		}
		log("argocd: refreshed " + app.Name)
		if err := syncApp(ctx, client, app, token); err != nil {
			err = fmt.Errorf("argocd: sync %s: %w", app.Name, err)
			log(err.Error())
			errs = append(errs, err)
			continue
		}
		log("argocd: sync triggered " + app.Name)
	}
	return errors.Join(errs...)
}

// newClient builds the one shared client — no client-wide Timeout (refresh
// legitimately blocks for minutes; each request carries its own deadline);
// insecure swaps in a clone of the default transport with TLS verification
// off.
func newClient(insecure bool) *http.Client {
	c := &http.Client{}
	if insecure {
		if t, ok := http.DefaultTransport.(*http.Transport); ok {
			t = t.Clone()
			if t.TLSClientConfig == nil {
				t.TLSClientConfig = &tls.Config{}
			}
			t.TLSClientConfig.InsecureSkipVerify = true
			c.Transport = t
		}
	}
	return c
}

func refresh(ctx context.Context, client *http.Client, app spec.ArgoApp, token string) error {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	u := appURL(app) + "?refresh=normal"
	if app.Namespace != "" {
		u += "&appNamespace=" + url.QueryEscape(app.Namespace)
	}
	return do(ctx, client, http.MethodGet, u, token, "")
}

func syncApp(ctx context.Context, client *http.Client, app spec.ArgoApp, token string) error {
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	body := "{}"
	if app.Namespace != "" {
		b, err := json.Marshal(struct {
			AppNamespace string `json:"appNamespace"`
		}{app.Namespace})
		if err != nil {
			return err
		}
		body = string(b)
	}
	return do(ctx, client, http.MethodPost, appURL(app)+"/sync", token, body)
}

// appURL is <server>/api/v1/applications/<name> with the name path-escaped
// (project-scoped names may contain characters unsafe in a path segment).
func appURL(app spec.ArgoApp) string {
	return strings.TrimSuffix(app.Server, "/") + "/api/v1/applications/" + url.PathEscape(app.Name)
}

// do issues one request and maps a non-2xx response to an error carrying the
// status and a trimmed body excerpt.
func do(ctx context.Context, client *http.Client, method, u, token, body string) error {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		excerpt := strings.TrimSpace(string(b))
		if len(excerpt) > maxExcerpt {
			excerpt = excerpt[:maxExcerpt] + "…"
		}
		if excerpt == "" {
			return errors.New(resp.Status)
		}
		return fmt.Errorf("%s: %s", resp.Status, excerpt)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain for connection reuse
	return nil
}
