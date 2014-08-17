package analyticsbadge

import (
	"appengine"
	"appengine/datastore"
	"appengine/memcache"
	"appengine/urlfetch"
	"code.google.com/p/goauth2/oauth"
	"code.google.com/p/google-api-go-client/analytics/v3"
	"encoding/json"
	"html/template"
	"io/ioutil"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

type Account struct {
	Username     string
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
}

func (a *Account) GetToken() *oauth.Token {
	if a.AccessToken == "" {
		return nil
	}
	return &oauth.Token{
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		Expiry:       a.Expiry,
	}
}

func (a *Account) SetToken(t *oauth.Token) {
	a.AccessToken = t.AccessToken
	if t.RefreshToken != "" {
		a.RefreshToken = t.RefreshToken
	}
	a.Expiry = t.Expiry
}

type Session struct {
	Id      string
	Account Account
	Loaded  Account
}

func (s *Session) Key(c appengine.Context) *datastore.Key {
	if s.Account.Username == "" {
		return datastore.NewIncompleteKey(c, "Account", nil)
	}
	return datastore.NewKey(c, "Account", s.Account.Username, 0, nil)
}

type Property struct {
	Account *datastore.Key
	Id      string
	Profile string
}

type Wrapper func(http.ResponseWriter, *http.Request, *Session) error

func (fn Wrapper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	s := &Session{}
	cookie, err := r.Cookie("session")
	if err == nil {
		s.Id = cookie.Value
		item, err := memcache.Get(c, "s:"+s.Id)
		if err == nil {
			s.Account = Account{
				Username: string(item.Value),
			}
			if err := datastore.Get(c, s.Key(c), &s.Account); err != nil {
				c.Errorf("datastore.Get error: %#v", err)
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}
			s.Loaded = s.Account
		}
	} else {
		s.Id = strconv.FormatInt(rand.Int63(), 36)
		cookie := &http.Cookie{
			Name:   "session",
			Value:  s.Id,
			MaxAge: 3600,
		}
		http.SetCookie(w, cookie)
	}
	if err := fn(w, r, s); err != nil {
		c.Errorf("Handler error: %#v", err)
		http.Error(w, err.Error(), 500)
		return
	}
	if s.Loaded != s.Account {
		item := &memcache.Item{
			Key:        "s:" + s.Id,
			Value:      []byte(s.Account.Username),
			Expiration: time.Hour,
		}
		if err := memcache.Set(c, item); err != nil {
			c.Errorf("Memcache write error: %#v", err)
		}
		_, err = datastore.Put(c, s.Key(c), &s.Account)
		if err != nil {
			c.Errorf("datastore.Put write error: %#v", err)
		}
	}
}

type Config struct {
	Web struct {
		AuthUri      string   `json:"auth_uri"`
		ClientId     string   `json:"client_id"`
		ClientSecret string   `json:"client_secret"`
		RedirectURIs []string `json:"redirect_uris"`
		TokenURI     string   `json:"token_uri"`
	}
}

var (
	config    oauth.Config
	templates = template.Must(template.ParseGlob("templates/[^.]*"))
)

func init() {
	rand.Seed(time.Now().UnixNano())
	// Retrieved from https://console.developers.google.com/project after enabling the analytics API.
	file, _ := ioutil.ReadFile("client_secrets.json")
	var parsed Config
	json.Unmarshal(file, &parsed)
	config = oauth.Config{
		AccessType:   "offline",
		Scope:        "https://www.googleapis.com/auth/analytics.readonly",
		AuthURL:      parsed.Web.AuthUri,
		ClientId:     parsed.Web.ClientId,
		ClientSecret: parsed.Web.ClientSecret,
		RedirectURL:  parsed.Web.RedirectURIs[0],
		TokenURL:     parsed.Web.TokenURI,
	}
	http.HandleFunc("/", index)
	http.HandleFunc("/badge/", badge)
	http.Handle("/manage", Wrapper(manage))
	http.Handle("/oauth", Wrapper(auth))
}

func manage(w http.ResponseWriter, r *http.Request, s *Session) error {
	c := appengine.NewContext(r)
	t := &oauth.Transport{Config: &config, Transport: &urlfetch.Transport{Context: c}}
	t.Token = s.Account.GetToken()
	if t.Token == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return nil
	}
	a, err := analytics.New(t.Client())
	if err != nil {
		return err
	}
	accounts, err := a.Management.AccountSummaries.List().Do()
	if err != nil {
		return err
	}
	loaded := make(map[string]bool)
	for _, account := range accounts.Items {
		for _, property := range account.WebProperties {
			loaded[property.Id] = true
		}
	}
	c.Infof("setting: %#v", t.Token)
	s.Account.SetToken(t.Token)
	if r.Method == "POST" {
		w.Header().Set("Content-Type", "text/html")
		r.ParseForm()
		var keys []*datastore.Key
		var properties []*Property
		var cache []string
		for id := range r.Form {
			if !loaded[id] {
				continue
			}
			profile := r.FormValue(id)
			p := &Property{
				Account: s.Key(c),
				Id:      id,
				Profile: profile,
			}
			keys = append(keys, datastore.NewKey(c, "Property", p.Id, 0, nil))
			properties = append(properties, p)
			cache = append(cache, "b:"+p.Id)
		}
		_, err := datastore.PutMulti(c, keys, properties)
		if err != nil {
			c.Errorf("datastore.PutMulti error: %#v", err)
		}
		if err = memcache.DeleteMulti(c, cache); err != nil {
			c.Errorf("memcache.DeleteMulti error: %#v", err)
		}
		http.Redirect(w, r, "/manage", http.StatusFound)
		return nil
	}
	w.Header().Set("Content-Type", "text/html")
	params := &struct {
		Accounts *analytics.AccountSummaries
		Profiles map[string]string
	}{
		accounts,
		make(map[string]string),
	}
	var properties []Property
	q := datastore.NewQuery("Property").Filter("Account =", s.Key(c))
	q.GetAll(c, &properties)
	for _, p := range properties {
		params.Profiles[p.Id] = p.Profile
	}
	templates.ExecuteTemplate(w, "manage.html", params)
	return nil
}

func auth(w http.ResponseWriter, r *http.Request, s *Session) error {
	c := appengine.NewContext(r)
	t := &oauth.Transport{Config: &config, Transport: &urlfetch.Transport{Context: c}}
	token := s.Account.GetToken()
	if token != nil {
		t.Token = token
	}
	t.Exchange(r.FormValue("code"))
	a, err := analytics.New(t.Client())
	if err != nil {
		return err
	}
	accounts, err := a.Management.AccountSummaries.List().Do()
	if err != nil {
		return err
	}
	// Error out if no associated properties?
	s.Account.Username = accounts.Username
	c.Infof("setting: %#v", t.Token)
	s.Account.SetToken(t.Token)
	http.Redirect(w, r, "/manage", http.StatusFound)
	return nil
}

func index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	templates.ExecuteTemplate(w, "index.html", config.AuthCodeURL(""))
}

func metric(i int) (string, string) {
	if i > 1000000 {
		return strconv.Itoa(i/1000000) + "M", "#4c1"
	}
	if i > 1000 {
		return strconv.Itoa(i/1000) + "k", "#a4a61d"
	}
	return strconv.Itoa(i), "#e05d44"
}

func size(s string) int {
	r := 10
	// Values from single letter SVG font rendering width, Chrome.
	for _, c := range s {
		switch c {
		case 'i':
			r += 2
		case ';', 'I', '\\', 'f', 'j', 'l', 'r', 't':
			r += 4
		case '1', '3', '5', '7', '9', ':', '?', 'E', 'F', 'J', 'P', 'T', 'Z', '[', ']', '`', 'b', 'c', 'd', 'g', 'k', 'o', 'p', 's', 'v', 'y':
			r += 6
		case 'K', 'L':
			r += 7
		case '<', '>', '@', 'G', 'O', 'W', 'm':
			r += 10
		default:
			r += 8
		}
	}
	return r
}

func badge(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	path := r.URL.Path[7 : len(r.URL.Path)-4]
	total := 0
	item, err := memcache.Get(c, "b:"+path)
	if err == nil {
		total, err = strconv.Atoi(string(item.Value))
		if err != nil {
			c.Errorf("badge(Memcache read) error: %#v", err)
			return
		}
	} else {
		k := datastore.NewKey(c, "Property", path, 0, nil)
		var p Property
		if err := datastore.Get(c, k, &p); err != nil {
			c.Errorf("badge(Property) error: %#v", err)
			return
		}
		var a Account
		if err := datastore.Get(c, p.Account, &a); err != nil {
			c.Errorf("badge(Account) error: %#v", err)
			return
		}
		loaded := a
		t := &oauth.Transport{Config: &config, Transport: &urlfetch.Transport{Context: c}}
		t.Token = a.GetToken()
		analytics, err := analytics.New(t.Client())
		if err != nil {
			c.Errorf("badge error: %#v", err)
			return
		}
		result, err := analytics.Data.Ga.Get("ga:"+p.Profile, "7daysAgo", "yesterday", "ga:users").Do()
		if err != nil {
			c.Errorf("badge(Data) error: %#v", err)
			return
		}
		total, err = strconv.Atoi(result.TotalsForAllResults["ga:users"])
		if err != nil {
			c.Errorf("badge(Total) error: %#v", err)
			return
		}
		item := &memcache.Item{
			Key:        "b:" + path,
			Value:      []byte(strconv.Itoa(total)),
			Expiration: time.Hour * 12,
		}
		if err := memcache.Set(c, item); err != nil {
			c.Errorf("badge(Memcache) error: %#v", err)
		}
		c.Infof("setting: %#v", t.Token)
		a.SetToken(t.Token)
		if a != loaded {
			_, err = datastore.Put(c, p.Account, &a)
		}
	}
	number, color := metric(total)
	params := &struct {
		Color       string
		Left        string
		Right       string
		LeftWidth   int
		RightWidth  int
		LeftCenter  int
		RightCenter int
		Total       int
	}{
		Left:  "users",
		Right: number + "/week",
		Color: color,
	}
	params.LeftWidth = size(params.Left)
	params.RightWidth = size(params.Right)
	params.Total = params.LeftWidth + params.RightWidth
	params.LeftCenter = params.LeftWidth/2 + 1
	params.RightCenter = params.LeftWidth + params.RightWidth/2 - 1
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	templates.ExecuteTemplate(w, "badge.svg", params)
}
