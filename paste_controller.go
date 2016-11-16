package main

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/DHowett/ghostbin/lib/formatting"
	"github.com/DHowett/ghostbin/model"

	"github.com/golang/glog"
	"github.com/golang/groupcache/lru"
	"github.com/gorilla/mux"
)

// paste http bindings
const CURRENT_ENCRYPTION_METHOD model.PasteEncryptionMethod = model.PasteEncryptionMethodAES_CTR
const PASTE_CACHE_MAX_ENTRIES int = 1000
const PASTE_MAXIMUM_LENGTH ByteSize = 1048576 // 1 MB
const MAX_EXPIRE_DURATION time.Duration = 15 * 24 * time.Hour

type PasteAccessDeniedError struct {
	action string
	ID     model.PasteID
}

func (e PasteAccessDeniedError) Error() string {
	return "You're not allowed to " + e.action + " paste " + e.ID.String()
}

// Make the various errors we can throw conform to HTTPError (here vs. the generic type file)
func (e PasteAccessDeniedError) StatusCode() int {
	return http.StatusForbidden
}

type PasteTooLargeError ByteSize

func (e PasteTooLargeError) Error() string {
	return fmt.Sprintf("Your input (%v) exceeds the maximum paste length, which is %v.", ByteSize(e), PASTE_MAXIMUM_LENGTH)
}

func (e PasteTooLargeError) StatusCode() int {
	return http.StatusBadRequest
}

type PasteController struct {
	App   Application
	Model model.Broker
}

func (pc *PasteController) getPasteFromRequest(r *http.Request) (model.Paste, error) {
	id := model.PasteIDFromString(mux.Vars(r)["id"])
	var passphrase []byte

	session := sessionBroker.Get(r)
	if pasteKeys, ok := session.Get(SessionScopeSensitive, "paste_passphrases").(map[model.PasteID][]byte); ok {
		if _key, ok := pasteKeys[id]; ok {
			passphrase = _key
		}
	}

	return pc.Model.GetPaste(id, passphrase)
}

type pasteHandlerFunc func(p model.Paste, w http.ResponseWriter, r *http.Request)

func (pc *PasteController) wrapPasteHandler(handler pasteHandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := model.PasteIDFromString(mux.Vars(r)["id"])
		p, err := pc.getPasteFromRequest(r)

		if err == model.ErrPasteEncrypted || err == model.ErrInvalidKey {
			url := pc.App.GenerateURL(URLTypePasteAuthenticate, "id", id.String())
			if err == model.ErrInvalidKey {
				url.Query().Set("i", "1")
			}

			http.SetCookie(w, &http.Cookie{
				Name:  "destination",
				Value: r.URL.String(),
				Path:  "/",
			})
			w.Header().Set("Location", url.String())
			w.WriteHeader(http.StatusFound)
		} else {
			if err != nil {
				if err == model.ErrNotFound {
					w.WriteHeader(http.StatusNotFound)
					templatePack.ExecutePage(w, r, "paste_not_found", id)
				} else {
					w.WriteHeader(http.StatusInternalServerError)
					templatePack.ExecutePage(w, r, "error", err)
				}
				return
			}

			handler(p, w, r)
		}
	})
}

func (pc *PasteController) wrapPasteEditHandler(fn pasteHandlerFunc) pasteHandlerFunc {
	return func(p model.Paste, w http.ResponseWriter, r *http.Request) {
		if !isEditAllowed(p, r) {
			accerr := PasteAccessDeniedError{"modify", p.GetID()}
			panic(accerr)
		}
		fn(p, w, r)
	}
}

func (pc *PasteController) getPasteJSONHandler(p model.Paste, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	reader, _ := p.Reader()
	defer reader.Close()
	buf := &bytes.Buffer{}
	io.Copy(buf, reader)

	pasteMap := map[string]interface{}{
		"id":         p.GetID(),
		"language":   p.GetLanguageName(),
		"encrypted":  p.IsEncrypted(),
		"expiration": p.GetExpiration(),
		"body":       string(buf.Bytes()),
	}

	json, _ := json.Marshal(pasteMap)
	w.Write(json)
}

func (pc *PasteController) getPasteRawHandler(p model.Paste, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "null")
	w.Header().Set("Vary", "Origin")

	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-XSS-Protection", "1; mode=block")

	ext := "txt"
	if mux.CurrentRoute(r).GetName() == "download" {
		lang := formatting.LanguageNamed(p.GetLanguageName())
		if lang != nil {
			if len(lang.Extensions) > 0 {
				ext = lang.Extensions[0]
			}
		}

		filename := p.GetID().String()
		if p.GetTitle() != "" {
			filename = p.GetTitle()
		}
		w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"."+ext+"\"")
		w.Header().Set("Content-Transfer-Encoding", "binary")
	}

	reader, _ := p.Reader()
	defer reader.Close()
	io.Copy(w, reader)
}

func (pc *PasteController) pasteGrantHandler(p model.Paste, w http.ResponseWriter, r *http.Request) {
	grant, _ := pc.Model.CreateGrant(p)

	acceptURL := pc.App.GenerateURL(URLTypePasteGrantAccept, "grantkey", string(grant.GetID()))

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.Encode(map[string]string{
		"acceptURL": BaseURLForRequest(r).ResolveReference(acceptURL).String(),
		"key":       string(grant.GetID()),
		"id":        p.GetID().String(),
	})
}

func (pc *PasteController) pasteUngrantHandler(p model.Paste, w http.ResponseWriter, r *http.Request) {
	GetPastePermissionScope(p.GetID(), r).Revoke(model.PastePermissionAll)
	SavePastePermissionScope(w, r)

	SetFlash(w, "success", fmt.Sprintf("Paste %v disavowed.", p.GetID()))
	w.Header().Set("Location", pasteURL("show", p.GetID()))
	w.WriteHeader(http.StatusSeeOther)
}

func (pc *PasteController) grantAcceptHandler(w http.ResponseWriter, r *http.Request) {
	v := mux.Vars(r)
	grantKey := model.GrantID(v["grantkey"])
	grant, err := pc.Model.GetGrant(grantKey)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("Hey."))
		return
	}

	pID := grant.GetPasteID()
	GetPastePermissionScope(pID, r).Grant(model.PastePermissionEdit)
	SavePastePermissionScope(w, r)

	grant.Destroy()

	SetFlash(w, "success", fmt.Sprintf("You now have edit rights to Paste %v.", pID))
	w.Header().Set("Location", pasteURL("show", pID))
	w.WriteHeader(http.StatusSeeOther)
}

func (pc *PasteController) pasteUpdate(p model.Paste, w http.ResponseWriter, r *http.Request) {
	pc.pasteUpdateCore(p, w, r, false)
}

func (pc *PasteController) pasteUpdateCore(p model.Paste, w http.ResponseWriter, r *http.Request, newPaste bool) {
	body := r.FormValue("text")
	if len(strings.TrimSpace(body)) == 0 {
		w.Header().Set("Location", pasteURL("delete", p.GetID()))
		w.WriteHeader(http.StatusFound)
		return
	}

	pasteLen := ByteSize(len(body))
	if pasteLen > PASTE_MAXIMUM_LENGTH {
		panic(PasteTooLargeError(pasteLen))
	}

	if !newPaste {
		// If this is an update (instead of a new paste), blow away the hash.
		tok := "P|H|" + p.GetID().String()
		v, _ := ephStore.Get(tok)
		if hash, ok := v.(string); ok {
			ephStore.Delete(hash)
			ephStore.Delete(tok)
		}
	}

	lang := formatting.LanguageNamed(p.GetLanguageName())
	pw, _ := p.Writer()
	pw.Write([]byte(body))
	if r.FormValue("lang") != "" {
		lang = formatting.LanguageNamed(r.FormValue("lang"))
	}

	if lang != nil {
		p.SetLanguageName(lang.ID)
	}

	expireIn := r.FormValue("expire")
	ePid := ExpiringPasteID(p.GetID())
	if expireIn != "" && expireIn != "-1" {
		dur, _ := ParseDuration(expireIn)
		if dur > MAX_EXPIRE_DURATION {
			dur = MAX_EXPIRE_DURATION
		}
		pasteExpirator.ExpireObject(ePid, dur)
	} else {
		if expireIn == "-1" && pasteExpirator.ObjectHasExpiration(ePid) {
			pasteExpirator.CancelObjectExpiration(ePid)
		}
	}

	p.SetExpiration(expireIn)

	p.SetTitle(r.FormValue("title"))

	pw.Close() // Saves p

	w.Header().Set("Location", pasteURL("show", p.GetID()))
	w.WriteHeader(http.StatusSeeOther)
}

func (pc *PasteController) pasteCreate(w http.ResponseWriter, r *http.Request) {
	session := sessionBroker.Get(r)

	body := r.FormValue("text")
	if len(strings.TrimSpace(body)) == 0 {
		// 400 here, 200 above (one is displayed to the user, one could be an API response.)
		RenderError(fmt.Errorf("Hey, put some text in that paste."), 400, w)
		return
	}

	pasteLen := ByteSize(len(body))
	if pasteLen > PASTE_MAXIMUM_LENGTH {
		err := PasteTooLargeError(pasteLen)
		RenderError(err, err.StatusCode(), w)
		return
	}

	password := r.FormValue("password")
	encrypted := password != ""

	if encrypted && (Env() != EnvironmentDevelopment && !RequestIsHTTPS(r)) {
		RenderError(fmt.Errorf("I refuse to accept passwords over HTTP."), 400, w)
		return
	}

	var p model.Paste
	var err error

	if !encrypted {
		// We can only hash-dedup non-encrypted pastes.
		hasher := md5.New()
		io.WriteString(hasher, body)
		hashToken := "H|" + SourceIPForRequest(r) + "|" + base32Encoder.EncodeToString(hasher.Sum(nil))

		v, _ := ephStore.Get(hashToken)
		if hashedPaste, ok := v.(model.Paste); ok {
			pc.pasteUpdateCore(hashedPaste, w, r, true)
			// TODO(DH) EARLY RETURN
			return
		}

		p, err = pc.Model.CreatePaste()
		if err != nil {
			panic(err)
		}

		ephStore.Put(hashToken, p, 5*time.Minute)
		ephStore.Put("P|H|"+p.GetID().String(), hashToken, 5*time.Minute)
	} else {
		p, err = pc.Model.CreateEncryptedPaste(CURRENT_ENCRYPTION_METHOD, []byte(password))
		if err != nil {
			panic(err)
		}

		sensitiveScope := session.Scope(SessionScopeSensitive)
		pasteKeys, ok := sensitiveScope.Get("paste_passphrases").(map[model.PasteID][]byte)
		if !ok {
			pasteKeys = map[model.PasteID][]byte{}
		}

		pasteKeys[p.GetID()] = []byte(password)
		sensitiveScope.Set("paste_passphrases", pasteKeys)
	}

	GetPastePermissionScope(p.GetID(), r).Grant(model.PastePermissionAll)
	SavePastePermissionScope(w, r)

	session.Save()

	pc.pasteUpdateCore(p, w, r, true)
}

func (pc *PasteController) pasteDelete(p model.Paste, w http.ResponseWriter, r *http.Request) {
	oldId := p.GetID()
	p.Erase()

	GetPastePermissionScope(oldId, r).Revoke(model.PastePermissionAll)
	SavePastePermissionScope(w, r)

	SetFlash(w, "success", fmt.Sprintf("Paste %v deleted.", oldId))

	redir := r.FormValue("redir")
	if redir == "reports" {
		w.Header().Set("Location", "/admin/reports")
	} else {
		w.Header().Set("Location", "/")
	}

	w.WriteHeader(http.StatusFound)
}

func (pc *PasteController) authenticatePastePOSTHandler(w http.ResponseWriter, r *http.Request) {
	if throttleAuthForRequest(r) {
		RenderError(fmt.Errorf("Cool it."), 420, w)
		return
	}

	id := model.PasteIDFromString(mux.Vars(r)["id"])
	passphrase := []byte(r.FormValue("password"))

	session := sessionBroker.Get(r).Scope(SessionScopeSensitive)
	pasteKeys, ok := session.Get("paste_passphrases").(map[model.PasteID][]byte)
	if !ok {
		pasteKeys = map[model.PasteID][]byte{}
	}

	pasteKeys[id] = passphrase
	session.Set("paste_passphrases", pasteKeys)
	session.Save()

	dest := pasteURL("show", id)
	if destCookie, err := r.Cookie("destination"); err != nil {
		dest = destCookie.Value
	}
	w.Header().Set("Location", dest)
	w.WriteHeader(http.StatusSeeOther)
}

// TODO(DH) MOVE
func throttleAuthForRequest(r *http.Request) bool {
	ip := SourceIPForRequest(r)

	id := mux.Vars(r)["id"]

	tok := ip + "|" + id

	var at *int32
	v, ok := ephStore.Get(tok)
	if v != nil && ok {
		at = v.(*int32)
	} else {
		var n int32
		at = &n
		ephStore.Put(tok, at, 1*time.Minute)
	}

	*at++

	if *at >= 5 {
		// If they've tried and failed too much, renew the throttle
		// at five minutes, to make them cool off.

		ephStore.Put(tok, at, 5*time.Minute)
		return true
	}

	return false
}

type renderedPaste struct {
	body       template.HTML
	renderTime time.Time
}

var renderCache struct {
	mu sync.RWMutex
	c  *lru.Cache
}

// TODO(DH) MOVE
func renderPaste(p model.Paste) template.HTML {
	renderCache.mu.RLock()
	var cached *renderedPaste
	var cval interface{}
	var ok bool
	if renderCache.c != nil {
		if cval, ok = renderCache.c.Get(p.GetID()); ok {
			cached = cval.(*renderedPaste)
		}
	}
	renderCache.mu.RUnlock()

	if !ok || cached.renderTime.Before(p.GetModificationTime()) {
		defer renderCache.mu.Unlock()
		renderCache.mu.Lock()
		out, err := FormatPaste(p)

		if err != nil {
			glog.Errorf("Render for %s failed: (%s) output: %s", p.GetID(), err.Error(), out)
			return template.HTML("There was an error rendering this paste.")
		}

		rendered := template.HTML(out)
		if !p.IsEncrypted() {
			if renderCache.c == nil {
				renderCache.c = &lru.Cache{
					MaxEntries: PASTE_CACHE_MAX_ENTRIES,
					OnEvicted: func(key lru.Key, value interface{}) {
						glog.Info("RENDER CACHE: Evicted ", key)
					},
				}
			}
			renderCache.c.Add(p.GetID(), &renderedPaste{body: rendered, renderTime: time.Now()})
			glog.Info("RENDER CACHE: Cached ", p.GetID())
		}

		return rendered
	} else {
		return cached.body
	}
}

func (pc *PasteController) generateRenderPageHandler(page string) pasteHandlerFunc {
	// We don't defer the error handler here because it happened a step up
	return func(p model.Paste, w http.ResponseWriter, r *http.Request) {
		templatePack.ExecutePage(w, r, page, p)
	}
}

func (pc *PasteController) InitRoutes(router *mux.Router) {
	router.Methods("GET").
		Path("/new").
		Handler(RedirectHandler("/"))

	router.Methods("POST").
		Path("/new").
		Handler(http.HandlerFunc(pc.pasteCreate))

	router.Methods("GET").
		Path("/{id}.json").
		Handler(pc.wrapPasteHandler(pc.getPasteJSONHandler))

	pasteShowRoute :=
		router.Methods("GET").
			Path("/{id}").
			Handler(pc.wrapPasteHandler(pc.generateRenderPageHandler("paste_show")))

	pasteGrantRoute :=
		router.Methods("POST").
			Path("/{id}/grant/new").
			Handler(pc.wrapPasteHandler(pc.wrapPasteEditHandler(pc.pasteGrantHandler)))

	pasteGrantAcceptRoute :=
		router.Methods("GET").
			Path("/grant/{grantkey}/accept").
			Handler(http.HandlerFunc(pc.grantAcceptHandler))

	router.Methods("GET").
		Path("/{id}/disavow").
		Handler(pc.wrapPasteHandler(pc.wrapPasteEditHandler(pc.pasteUngrantHandler)))

	pasteRawRoute :=
		router.Methods("GET").
			Path("/{id}/raw").
			Handler(pc.wrapPasteHandler(pc.getPasteRawHandler))

	pasteDownloadRoute :=
		router.Methods("GET").
			Path("/{id}/download").
			Handler(pc.wrapPasteHandler(pc.getPasteRawHandler))

	pasteEditRoute :=
		router.Methods("GET").
			Path("/{id}/edit").
			Handler(pc.wrapPasteHandler(pc.wrapPasteEditHandler(pc.generateRenderPageHandler("paste_edit"))))
	router.Methods("POST").
		Path("/{id}/edit").
		Handler(pc.wrapPasteHandler(pc.wrapPasteEditHandler(pc.pasteUpdate)))

	pasteDeleteRoute :=
		router.Methods("GET").
			Path("/{id}/delete").
			Handler(pc.wrapPasteHandler(pc.wrapPasteEditHandler(pc.generateRenderPageHandler("paste_delete_confirm"))))

	router.Methods("POST").
		Path("/{id}/delete").
		Handler(pc.wrapPasteHandler(pc.wrapPasteEditHandler(pc.pasteDelete)))

	pasteReportRoute :=
		router.Methods("POST").
			Path("/{id}/report").
			Handler(pc.wrapPasteHandler(reportPaste))

	pasteAuthenticateRoute :=
		router.Methods("GET").
			MatcherFunc(HTTPSMuxMatcher).
			Path("/{id}/authenticate").
			Handler(RenderPageHandler("paste_authenticate"))
	router.Methods("POST").
		MatcherFunc(HTTPSMuxMatcher).
		Path("/{id}/authenticate").
		Handler(http.HandlerFunc(pc.authenticatePastePOSTHandler))

	router.Methods("GET").
		MatcherFunc(NonHTTPSMuxMatcher).
		Path("/{id}/authenticate").
		Handler(RenderPageHandler("paste_authenticate_disallowed"))

	// catch-all rule that redirects paste/ to /
	router.Methods("GET").Path("/").Handler(RedirectHandler("/"))

	pc.App.RegisterRouteForURLType(URLTypePasteShow, pasteShowRoute)
	pc.App.RegisterRouteForURLType(URLTypePasteGrant, pasteGrantRoute)
	pc.App.RegisterRouteForURLType(URLTypePasteGrantAccept, pasteGrantAcceptRoute)
	pc.App.RegisterRouteForURLType(URLTypePasteRaw, pasteRawRoute)
	pc.App.RegisterRouteForURLType(URLTypePasteDownload, pasteDownloadRoute)
	pc.App.RegisterRouteForURLType(URLTypePasteEdit, pasteEditRoute)
	pc.App.RegisterRouteForURLType(URLTypePasteDelete, pasteDeleteRoute)
	pc.App.RegisterRouteForURLType(URLTypePasteReport, pasteReportRoute)
	pc.App.RegisterRouteForURLType(URLTypePasteAuthenticate, pasteAuthenticateRoute)
}

func NewPasteController(app Application, modelBroker model.Broker) Controller {
	return &PasteController{
		App:   app,
		Model: modelBroker,
	}
}
