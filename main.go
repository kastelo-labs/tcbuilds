package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
)

var (
	base         = "https://build.kastelo.net"
	branch       = "master"
	listen       = "127.0.0.1:8123"
	auth         = ""
	maxCacheTime = 5 * time.Minute
	projectName  = ""
	title        = ""
)

func main() {
	flag.StringVar(&base, "base", base, "TeamCity server address")
	flag.StringVar(&branch, "branch", branch, "Branch to show")
	flag.StringVar(&listen, "listen", listen, "Server listen address")
	flag.StringVar(&projectName, "project", projectName, "Top level project")
	flag.StringVar(&auth, "auth", auth, "username:password")
	flag.StringVar(&title, "title", title, "Custom page title")
	flag.DurationVar(&maxCacheTime, "cache", maxCacheTime, "Cache life time")
	flag.Parse()

	http.HandleFunc("/", handler)
	http.HandleFunc("/refresh/", refresh)

	go refreshLoop()
	refreshRequests <- struct{}{}

	http.ListenAndServe(listen, nil)
}

var (
	refreshRequests = make(chan struct{}, 1)
	cacheData       []byte
	cacheMut        sync.Mutex
)

func handler(w http.ResponseWriter, req *http.Request) {
	cacheMut.Lock()
	bs := cacheData
	cacheMut.Unlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(bs)
}

func refresh(_ http.ResponseWriter, _ *http.Request) {
	select {
	case refreshRequests <- struct{}{}:
	default:
	}
}

func refreshLoop() {
	for _ = range refreshRequests {
		refreshCache()
	}
}

func refreshCache() {
	t0 := time.Now()
	defer func() {
		log.Println("Done in", time.Since(t0))
	}()

	cacheMut.Lock()
	defer cacheMut.Unlock()

	log.Println("Refresh cache")
	bs, err := getTpl()
	if err != nil {
		log.Println(err)
	}

	cacheData = bs
}

func getTpl() ([]byte, error) {
	types, err := getBuildTypes()
	if err != nil {
		return nil, errors.Wrap(err, "getTpl")
	}

	sort.Slice(types, func(a, b int) bool {
		if types[a].ProjectName != types[b].ProjectName {
			return types[a].ProjectName < types[b].ProjectName
		}
		return types[a].Name < types[b].Name
	})

	var projs []project
	projIdxs := make(map[string]int)

	for _, bt := range types {
		idx, ok := projIdxs[bt.ProjectName]
		if !ok {
			idx = len(projs)
			projIdxs[bt.ProjectName] = idx
			projs = append(projs, project{Name: bt.ProjectName})
		}

		build, err := getLatestBuild(bt.ID, branch)
		if err != nil {
			continue
		}

		files, err := getFiles(build.ID)
		if err != nil {
			continue
		}

		build.Files = files
		bt.Build = build
		projs[idx].Builds = append(projs[idx].Builds, bt)
	}

	data := map[string]interface{}{
		"Branch":   branch,
		"Base":     base,
		"Projects": projs,
		"Title":    title,
	}
	buf := new(bytes.Buffer)
	if err := tpl.Execute(buf, data); err != nil {
		return nil, errors.Wrap(err, "execute template")
	}

	return buf.Bytes(), nil
}

type project struct {
	Name   string
	Builds []buildType
}

func (p project) NameID() string {
	return strings.Replace(p.Name, " ", "-", -1)
}

type buildTypeResponse struct {
	Count      int
	HRef       string
	BuildTypes []buildType `json:"buildType"`
}

type buildType struct {
	ID          string
	Name        string
	ProjectName string
	ProjectID   string
	HRef        string
	WebURL      string

	Build build // filled in later
}

type buildResponse struct {
	Count    int
	HRef     string
	NextHRef string
	Builds   []build `json:"build"`
}

type build struct {
	ID            int
	BuildTypeID   string
	Number        string
	State         string
	BranchName    string
	DefaultBranch bool
	HRef          string
	WebURL        string
	StatusText    string
	QueuedDate    string
	StartDate     string
	FinishDate    string
	Agent         struct {
		Name string
	}

	Files []file // filled in later
}

func (b build) DateStr() string {
	d, _ := time.Parse("20060102T150405-0700", b.FinishDate)
	return d.UTC().Format("2006-01-02 15:04:05 MST")
}

type artifactResponse struct {
	Count int
	Files []file `json:"file"`
}

type file struct {
	Name             string
	Size             int
	ModificationTime string
	HRef             string
	Content          struct {
		HRef string
	}
}

func (f file) SizeStr() string {
	const (
		_ = 1 << (10 * iota)
		KiB
		MiB
	)
	if f.Size >= MiB {
		mib := float64(f.Size) / MiB
		return fmt.Sprintf("%.02f MiB", mib)
	}
	kib := float64(f.Size) / KiB
	return fmt.Sprintf("%.01f KiB", kib)
}

var tpl = template.Must(template.New("index.html").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
{{if .Title}}
<title>{{.Title}}</title>
{{else}}
<title>Latest builds of {{.Branch}}</title>
{{end}}
<link rel="stylesheet" href="https://maxcdn.bootstrapcdn.com/bootstrap/4.0.0-alpha.6/css/bootstrap.min.css" integrity="sha384-rwoIResjU2yc3z8GV/NPeZWAv56rSmLldC3R/AZzGRnGxQQKnKkoFVhFQhNUwEyJ" crossorigin="anonymous">
<script src="https://code.jquery.com/jquery-3.1.1.slim.min.js" integrity="sha384-A7FZj7v+d/sdmMqp/nOQwliLvUsJfDHW+k9Omg/a/EheAdgtzNs3hpfag6Ed950n" crossorigin="anonymous"></script>
<script src="https://cdnjs.cloudflare.com/ajax/libs/tether/1.4.0/js/tether.min.js" integrity="sha384-DztdAPBWPRXSA/3eYEEUWrWCy7G5KFbe8fFjk5JAIxUYHKkDx6Qin1DkWx51bBrb" crossorigin="anonymous"></script>
<script src="https://maxcdn.bootstrapcdn.com/bootstrap/4.0.0-alpha.6/js/bootstrap.min.js" integrity="sha384-vBWWzlZJ8ea9aCX4pEW3rVHjgjt7zpkNpZk+02D9phzyeVkE+jo0ieGizqPLForn" crossorigin="anonymous"></script>
<style type="text/css">
body {
	margin: 4em;
}
h1, h2, h3 {
	margin-bottom: 0.5em;
}
hr {
	margin-top: 1.5em;
	margin-bottom: 1.5em;
}
</style>
</head>
<body>
<div class="container">
<div class="row">
<div class="col">
{{if .Title}}
<h1>{{.Title}}</h1>
{{else}}
<h1>Latest builds of <code>{{.Branch}}</code></h1>
{{end}}
{{range $idx, $proj := .Projects}}
	{{if $proj.Builds}}
		{{if gt $idx 0}}<hr/>{{end}}
		<h2 id="{{$proj.NameID}}">{{$proj.Name}}</h2>
		{{range $proj.Builds}}
			{{if .Build.Files}}
				<h4>{{.Name}} <a href="{{.Build.WebURL}}">#{{.Build.Number}}</a></h4>
				<p>
				Status: {{.Build.StatusText}}<br>
				Completed: {{.Build.DateStr}}<br>
				</p>
				<ul>
				{{range .Build.Files}}
					<li><a href="{{$.Base}}{{.Content.HRef}}">{{.Name}}</a> ({{.SizeStr}})
				{{end}}
				</ul>
			{{end}}
		{{end}}
	{{end}}
{{end}}
<hr>
<p class="text-muted">Served by <a href="https://kastelo.io/tcbuilds">kastelo.io/tcbuilds</a>.
</div>
</div>
</div>
</body>
</html>`))

func getBuildTypes() ([]buildType, error) {
	extra := ""
	if projectName != "" {
		extra = "?locator=affectedProject:(id:" + projectName + ")"
	}
	url := fmt.Sprintf("/app/rest/buildTypes%s", extra)
	var res buildTypeResponse
	if err := getJSON(url, &res); err != nil {
		return nil, errors.Wrap(err, "get build types")
	}
	return res.BuildTypes, nil
}

func getLatestBuild(buildTypeID, branch string) (build, error) {
	url := fmt.Sprintf("/app/rest/buildTypes/id:%s/builds?locator=branch:%s,state:finished,status:SUCCESS,count:1", buildTypeID, branch)
	var res buildResponse
	if err := getJSON(url, &res); err != nil {
		return build{}, errors.Wrap(err, "get latest build")
	}
	if len(res.Builds) != 1 {
		return build{}, errors.New("no build found")
	}

	// re-get the build for more info

	var b build
	if err := getJSON(res.Builds[0].HRef, &b); err != nil {
		return build{}, errors.Wrap(err, "get latest build details")
	}

	return b, nil
}

func getFiles(buildID int) ([]file, error) {
	url := fmt.Sprintf("/app/rest/builds/id:%d/artifacts/children", buildID)
	var res artifactResponse
	if err := getJSON(url, &res); err != nil {
		return nil, errors.Wrap(err, "get files")
	}
	return res.Files, nil
}

func getJSON(url string, into interface{}) error {
	authPart := ""
	switch {
	case strings.HasPrefix(url, "/guestAuth"):
	case strings.HasPrefix(url, "/httpAuth"):
	case auth != "":
		authPart = "/httpAuth"
	default:
		authPart = "/guestAuth"
	}

	req, err := http.NewRequest(http.MethodGet, base+authPart+url, nil)
	if err != nil {
		return errors.Wrap(err, "create request")
	}

	req.Header.Set("Accept", "application/json")
	if auth != "" {
		fields := strings.Split(auth, ":")
		if len(fields) == 2 {
			req.SetBasicAuth(fields[0], fields[1])
		}
	}

	log.Println(req.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "HTTP get")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}

	bs, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "HTTP read")
	}

	return errors.Wrap(json.Unmarshal(bs, into), "JSON unmarshal")
}
