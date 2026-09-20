package httpserver

import (
	"context"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"sci1.uk/catical/internal/fetch"
	"sci1.uk/catical/internal/icalmerge"
	"sci1.uk/catical/internal/store"
	"sci1.uk/catical/internal/tokens"
)

const mdcCSSURL = "https://unpkg.com/material-components-web@14.0.0/dist/material-components-web.min.css"

const htmlCSPBase = "default-src 'none'; style-src 'unsafe-inline' https://unpkg.com https://fonts.googleapis.com; font-src https://fonts.gstatic.com https://fonts.googleapis.com; img-src 'none'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

const htmlCSPNoJS = htmlCSPBase + "; connect-src 'none'; script-src 'none'"

const htmlCSPTurnstile = htmlCSPBase + "; connect-src " + turnstileOrigin + "; script-src " + turnstileOrigin + "; frame-src " + turnstileOrigin

func writeHTMLHeaders(w http.ResponseWriter, siteKey string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	csp := htmlCSPNoJS
	if siteKey != "" {
		csp = htmlCSPTurnstile
	}
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Robots-Tag", "noindex")
}

type pageData struct {
	Title           string
	Error           string
	Notice          string
	Name            string
	Prefix          bool
	Slots           []slot
	FeedURL         string
	ManageURL       string
	NewURL          string
	FormAction      string
	Warning         string
	SiteKey         string
	TurnstileAction string
	Commit          string
	CommitURL       string
	SourceURL       string
	FAQBody         template.HTML
}

//go:embed faq.html
var faqHTML string

const sourceRepoURL = "https://github.com/kittsville/CatiCal"

func (c Config) commitSHA() string {
	s := strings.TrimSpace(c.Commit)
	if s == "" {
		return "latest"
	}
	return s
}

func withFooter(cfg Config, d pageData) pageData {
	sha := cfg.commitSHA()
	short := sha
	if len(short) > 6 {
		short = short[:6]
	}
	d.Commit = short
	d.CommitURL = sourceRepoURL + "/commit/" + sha
	d.SourceURL = sourceRepoURL
	return d
}

type slot struct {
	URL   string
	Label string
}

func emptySlots() []slot {
	return make([]slot, fetch.MaxSources)
}

func slotsFromFeed(f store.Feed) []slot {
	out := emptySlots()
	for i, s := range f.Sources {
		if i >= fetch.MaxSources {
			break
		}
		out[i] = slot{URL: s.URL, Label: s.Label}
	}
	return out
}

func serveFAQ(w http.ResponseWriter, cfg Config) {
	writeHTMLHeaders(w, "")
	_ = faqTmpl.Execute(w, withFooter(cfg, pageData{
		Title:   "FAQ",
		FAQBody: template.HTML(faqHTML),
	}))
}

func serveCreateForm(w http.ResponseWriter, cfg Config, errMsg string, status int) {
	writeHTMLHeaders(w, cfg.TurnstileSiteKey)
	if errMsg != "" {
		if status == 0 {
			status = http.StatusBadRequest
		}
		w.WriteHeader(status)
	}
	_ = formTmpl.Execute(w, withFooter(cfg, pageData{
		Title:           "CatiCal - Combine Calendars",
		Error:           errMsg,
		Slots:           emptySlots(),
		Prefix:          false,
		SiteKey:         cfg.TurnstileSiteKey,
		TurnstileAction: turnstileCreate,
	}))
}

func serveCreate(w http.ResponseWriter, r *http.Request, cfg Config) {
	if cfg.Admin == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		serveCreateForm(w, cfg, "invalid form", http.StatusBadRequest)
		return
	}
	if !checkTurnstile(w, r, cfg, turnstileCreate, func(msg string, code int) {
		serveCreateForm(w, cfg, msg, code)
	}) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		serveCreateForm(w, cfg, "name is required", http.StatusBadRequest)
		return
	}
	srcs, err := parseFormSources(r.Context(), r, cfg)
	if errors.Is(err, errOriginRateLimited) {
		tooMany(w)
		return
	}
	if err != nil {
		serveCreateForm(w, cfg, err.Error(), http.StatusBadRequest)
		return
	}

	feedSalt, feedSecret, feedHash, err := tokens.Generate()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	manageSalt, manageSecret, manageHash, err := tokens.Generate()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	feed, err := cfg.Admin.CreateFeed(r.Context(), store.CreateFeedParams{
		Name:            name,
		PrefixSummaries: r.FormValue("prefix_summaries") != "",
		FeedTokenSalt:   feedSalt,
		FeedTokenHash:   feedHash,
		ManageTokenSalt: manageSalt,
		ManageTokenHash: manageHash,
		LastRequestAt:   cfg.Now(),
		Sources:         srcs,
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	base := cfg.baseURL()
	data := pageData{
		Title:     "Created combined calendar",
		FeedURL:   feedURL(base, feed.ID.String(), feedSecret),
		ManageURL: manageURL(base, feed.ID.String(), manageSecret),
		Warning:   "Copy both links now. The feed URL will not be shown again. If you lose the manage link it cannot be recovered.",
	}
	writeHTMLHeaders(w, cfg.TurnstileSiteKey)
	_ = createdTmpl.Execute(w, withFooter(cfg, data))
}

func serveManageGET(w http.ResponseWriter, r *http.Request, cfg Config) {
	feed, _, ok := loadManaged(w, r, cfg)
	if !ok {
		return
	}
	renderManage(w, cfg, http.StatusOK, pageData{
		Title:      "Managed combined calendar",
		Name:       feed.Name,
		Prefix:     feed.PrefixSummaries,
		Slots:      slotsFromFeed(feed),
		FormAction: r.URL.Path,
		Notice:     sourceErrors(feed),
	})
}

func serveManagePOST(w http.ResponseWriter, r *http.Request, cfg Config) {
	feed, _, ok := loadManaged(w, r, cfg)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		renderManage(w, cfg, http.StatusBadRequest, pageData{
			Title: "Managed combined calendar", Error: "invalid form",
			Name: feed.Name, Prefix: feed.PrefixSummaries, Slots: slotsFromFeed(feed),
			FormAction: r.URL.Path,
		})
		return
	}
	if !checkTurnstile(w, r, cfg, turnstileManage, func(msg string, code int) {
		renderManage(w, cfg, code, pageData{
			Title: "Managed combined calendar", Error: msg,
			Name: feed.Name, Prefix: feed.PrefixSummaries, Slots: slotsFromFeed(feed),
			FormAction: r.URL.Path,
		})
	}) {
		return
	}
	switch r.FormValue("action") {
	case "save":
		name := strings.TrimSpace(r.FormValue("name"))
		if name == "" {
			renderManage(w, cfg, http.StatusBadRequest, pageData{
				Title: "Managed combined calendar", Error: "name is required",
				Name: feed.Name, Prefix: feed.PrefixSummaries, Slots: slotsFromFeed(feed),
				FormAction: r.URL.Path,
			})
			return
		}
		srcs, err := parseFormSources(r.Context(), r, cfg)
		if errors.Is(err, errOriginRateLimited) {
			tooMany(w)
			return
		}
		if err != nil {
			renderManage(w, cfg, http.StatusBadRequest, pageData{
				Title: "Managed combined calendar", Error: err.Error(),
				Name: name, Prefix: r.FormValue("prefix_summaries") != "", Slots: slotsFromFeed(feed),
				FormAction: r.URL.Path,
			})
			return
		}
		err = cfg.Admin.UpdateFeed(r.Context(), feed.ID, store.UpdateFeedParams{
			Name:            name,
			PrefixSummaries: r.FormValue("prefix_summaries") != "",
			Sources:         srcs,
		})
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		updated, err := cfg.Admin.GetFeed(r.Context(), feed.ID)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		renderManage(w, cfg, http.StatusOK, pageData{
			Title: "Managed combined calendar", Notice: "Saved.",
			Name: updated.Name, Prefix: updated.PrefixSummaries, Slots: slotsFromFeed(updated),
			FormAction: r.URL.Path,
		})
	case "delete":
		if err := cfg.Admin.DeleteFeed(r.Context(), feed.ID); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Location", "/")
		w.WriteHeader(http.StatusSeeOther)
	case "rotate_feed":
		salt, sec, hash, err := tokens.Generate()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := cfg.Admin.RotateFeedToken(r.Context(), feed.ID, salt, hash); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeHTMLHeaders(w, cfg.TurnstileSiteKey)
		_ = rotateTmpl.Execute(w, withFooter(cfg, pageData{
			Title:      "Feed URL rotated",
			NewURL:     feedURL(cfg.baseURL(), feed.ID.String(), sec),
			FormAction: r.URL.Path,
			Warning:    "Copy the new feed URL now. It will not be shown again. Your manage URL is unchanged.",
		}))
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func renderManage(w http.ResponseWriter, cfg Config, status int, data pageData) {
	data.SiteKey = cfg.TurnstileSiteKey
	data.TurnstileAction = turnstileManage
	writeHTMLHeaders(w, cfg.TurnstileSiteKey)
	w.WriteHeader(status)
	_ = manageTmpl.Execute(w, withFooter(cfg, data))
}

func loadManaged(w http.ResponseWriter, r *http.Request, cfg Config) (store.Feed, []byte, bool) {
	if cfg.Admin == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return store.Feed{}, nil, false
	}
	id, secret, err := parseIDAndSecret(r)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return store.Feed{}, nil, false
	}
	feed, err := cfg.Admin.GetFeed(r.Context(), id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return store.Feed{}, nil, false
	}
	if !tokens.Verify(feed.ManageTokenSalt, feed.ManageTokenHash, secret) {
		http.Error(w, "not found", http.StatusNotFound)
		return store.Feed{}, nil, false
	}
	return feed, secret, true
}

func parseIDAndSecret(r *http.Request) (uuid.UUID, []byte, error) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return uuid.Nil, nil, err
	}
	raw := strings.TrimSuffix(r.PathValue("secret"), ".ics")
	secret, err := hex.DecodeString(raw)
	if err != nil {
		return uuid.Nil, nil, err
	}
	return id, secret, nil
}

func parseFormSources(ctx context.Context, r *http.Request, cfg Config) ([]store.CreateSource, error) {
	urls := r.Form["url"]
	labels := r.Form["label"]
	out := make([]store.CreateSource, 0, len(urls))
	for i, raw := range urls {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		label := ""
		if i < len(labels) {
			label = strings.TrimSpace(labels[i])
		}
		if len(out) >= fetch.MaxSources {
			return nil, fmt.Errorf("at most %d sources", fetch.MaxSources)
		}
		if err := validateHTTPSSource(ctx, cfg, raw); err != nil {
			return nil, err
		}
		out = append(out, store.CreateSource{URL: raw, Label: label, Position: len(out)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one source URL is required")
	}
	if cfg.originLim != nil && !cfg.originLim.allowN(peerIP(r), len(out)) {
		return nil, errOriginRateLimited
	}
	rawURLs := make([]string, len(out))
	for i, s := range out {
		rawURLs[i] = s.URL
	}
	results := fetchOrigins(ctx, cfg, rawURLs)
	for _, res := range results {
		if res.Err != nil {
			return nil, fmt.Errorf("could not download a source calendar")
		}
		if err := icalmerge.Parse(res.Body); err != nil {
			return nil, fmt.Errorf("a source URL is not a valid iCalendar")
		}
	}
	return out, nil
}

func fetchOrigins(ctx context.Context, cfg Config, urls []string) []fetch.Result {
	if cfg.Fetch != nil {
		return cfg.Fetch.GetAll(ctx, urls)
	}
	return fetch.GetAll(ctx, urls)
}

func validateHTTPSSource(ctx context.Context, cfg Config, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid URL")
	}
	if strings.ToLower(u.Scheme) != "https" {
		return fmt.Errorf("source URLs must be https")
	}
	if isCatiCalURL(cfg, u) {
		return fmt.Errorf("CatiCal feeds cannot be used as sources")
	}
	if err := fetch.ValidateURL(ctx, raw); err != nil {
		return fmt.Errorf("source URL not allowed")
	}
	return nil
}

func isCatiCalURL(cfg Config, u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	for _, raw := range []string{cfg.baseURL(), defaultBaseURL} {
		bu, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if strings.ToLower(bu.Hostname()) == host {
			return true
		}
	}
	return false
}

func sourceErrors(f store.Feed) string {
	var b strings.Builder
	for _, s := range f.Sources {
		if s.LastError != nil && *s.LastError != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			label := s.Label
			if label == "" {
				label = s.URL
			}
			b.WriteString(label)
			b.WriteString(": ")
			b.WriteString(*s.LastError)
		}
	}
	return b.String()
}

func feedURL(base, id string, secret []byte) string {
	return base + "/c/" + id + "/" + hex.EncodeToString(secret) + ".ics"
}

func manageURL(base, id string, secret []byte) string {
	return base + "/m/" + id + "/" + hex.EncodeToString(secret)
}

const layoutCSS = `*,*::before,*::after{box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
html,body{margin:0;max-width:100%}
main,footer{width:100%;max-width:42rem;margin:1.5rem auto;padding:0 1rem;min-width:0}
footer{margin-top:2rem;margin-bottom:1rem;font-size:.9rem;overflow-wrap:anywhere}
h1.mdc-typography--headline1{font-size:clamp(1.75rem,6vw,2.75rem);line-height:1.15;font-weight:400;letter-spacing:normal;margin:.25rem 0 .75rem;overflow-wrap:anywhere}
p,label,dd,dt{overflow-wrap:anywhere;max-width:100%}
label{display:block;margin:.6rem 0 .2rem}
input[type=text],input[type=url]{display:block;width:100%;max-width:100%;padding:.4rem}
.row{display:grid;grid-template-columns:minmax(0,1fr) minmax(5rem,8rem);gap:.5rem;margin:.5rem 0;}
.row>*{min-width:0;max-width:100%}
.err{color:#a40000}
.warn{background:#fff3cd;padding:.75rem;border:1px solid #c9a227;overflow-wrap:anywhere}
code,pre{word-break:break-all;white-space:pre-wrap;overflow-wrap:anywhere;max-width:100%}
.mdc-button{margin:.4rem .4rem 0 0}
.actions{display:flex;flex-wrap:wrap;align-items:stretch;gap:.4rem}
.cf-turnstile{max-width:100%;overflow-x:auto}
dt{font-weight:500;margin-top:1rem}
dd{margin:.35rem 0 0 0}
@media (max-width:36rem){
.row{grid-template-columns:1fr}
.actions{flex-direction:column}
.actions .mdc-button,.mdc-button{width:100%;margin:.4rem 0 0}
main,footer{padding:0 .75rem}
}`

func htmlShell(inner string) string {
	return `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><meta name="robots" content="noindex"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Title}}</title>` +
		`<link href="https://fonts.googleapis.com/icon?family=Material+Icons" rel="stylesheet">` +
		`<link href="https://fonts.googleapis.com/css?family=Roboto:300,400,500" rel="stylesheet">` +
		`<link href="` + mdcCSSURL + `" rel="stylesheet">` +
		`<style>` + layoutCSS + `</style>` +
		`{{if .SiteKey}}<script src="` + turnstileScript + `" async defer></script>{{end}}` +
		`</head>` +
		`<body class="mdc-typography"><main>` + inner + `</main>` +
		`<footer>v<a href="{{.CommitURL}}">{{.Commit}}</a> | <a href="{{.SourceURL}}">Source Code</a> | <a href="/faq">FAQ</a></footer>` +
		`</body></html>`
}

var (
	formTmpl    = template.Must(template.New("form").Parse(htmlShell(formBody)))
	createdTmpl = template.Must(template.New("created").Parse(htmlShell(createdBody)))
	manageTmpl  = template.Must(template.New("manage").Parse(htmlShell(manageBody)))
	rotateTmpl  = template.Must(template.New("rotate").Parse(htmlShell(rotateBody)))
	faqTmpl     = template.Must(template.New("faq").Parse(htmlShell(`{{.FAQBody}}`)))
)

const mdcRaised = `class="mdc-button mdc-button--raised" type="submit"`
const mdcOutlined = `class="mdc-button mdc-button--outlined" type="submit"`

const formBody = `
<h1 class="mdc-typography mdc-typography--headline1">Catical</h1>
<p>A tool for merging calendar feeds. Combine multiple iCal feeds into a single sharable URL.</p>
<p>Confused? Learn more in the <a href="/faq">FAQ</a>.</p>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<form method="post" action="/">
<label>Name <input type="text" name="name" value="{{.Name}}" required></label>
<label><input type="checkbox" name="prefix_summaries" {{if .Prefix}}checked{{end}}> Prefix event summaries with source labels</label>
<p>Sources (https URLs)</p>
{{range .Slots}}
<div class="row">
<input type="url" name="url" placeholder="https://…" value="{{.URL}}">
<input type="text" name="label" placeholder="label" value="{{.Label}}">
</div>
{{end}}
{{if .SiteKey}}
<div class="cf-turnstile" data-sitekey="{{.SiteKey}}" data-action="{{.TurnstileAction}}"></div>
{{end}}
<p><button ` + mdcRaised + `><span class="mdc-button__label">Combine Calendars</span></button></p>
</form>`

const createdBody = `
<h1 class="mdc-typography mdc-typography--headline1">Calendars combined</h1>
<p class="warn">{{.Warning}}</p>
<p>Feed (subscribe in a calendar app):</p>
<p><code>{{.FeedURL}}</code></p>
<p>Manage (bookmark this; it is unrecoverable):</p>
<p><code>{{.ManageURL}}</code></p>
<p><a href="{{.ManageURL}}">Open management page</a></p>`

const manageBody = `
<h1 class="mdc-typography mdc-typography--headline1">Manage mix</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if .Notice}}<pre>{{.Notice}}</pre>{{end}}
<form method="post" action="{{.FormAction}}">
<label>Name <input type="text" name="name" value="{{.Name}}" required></label>
<label><input type="checkbox" name="prefix_summaries" {{if .Prefix}}checked{{end}}> Prefix event summaries with source labels</label>
<p>Sources (https URLs)</p>
{{range .Slots}}
<div class="row">
<input type="url" name="url" value="{{.URL}}">
<input type="text" name="label" placeholder="label" value="{{.Label}}">
</div>
{{end}}
{{if .SiteKey}}
<div class="cf-turnstile" data-sitekey="{{.SiteKey}}" data-action="{{.TurnstileAction}}"></div>
{{end}}
<p class="actions">
<button ` + mdcRaised + ` name="action" value="save"><span class="mdc-button__label">Save</span></button>
<button ` + mdcOutlined + ` name="action" value="delete"><span class="mdc-button__label">Delete</span></button>
<button ` + mdcOutlined + ` name="action" value="rotate_feed"><span class="mdc-button__label">Reset feed URL</span></button>
</p>
</form>
<p>The ICS feed URL is not shown here. Rotate the feed token if it leaked.</p>`

const rotateBody = `
<h1 class="mdc-typography mdc-typography--headline1">{{.Title}}</h1>
<p class="warn">{{.Warning}}</p>
<p><code>{{.NewURL}}</code></p>
{{if .FormAction}}<p><a href="{{.FormAction}}">Back to manage</a></p>{{end}}
<p>&#60; <a href="/">Home</a></p>`
