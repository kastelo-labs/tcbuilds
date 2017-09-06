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
	"os"
	"path/filepath"
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
	maxCacheTime = 24 * time.Hour
	projectName  = ""
	templateFile = "template.html"
	tpl          *template.Template
)

func main() {
	flag.StringVar(&base, "base", base, "TeamCity server address")
	flag.StringVar(&branch, "branch", branch, "Branch to show")
	flag.StringVar(&listen, "listen", listen, "Server listen address")
	flag.StringVar(&projectName, "project", projectName, "Top level project")
	flag.StringVar(&auth, "auth", auth, "username:password")
	flag.DurationVar(&maxCacheTime, "cache", maxCacheTime, "Cache life time")
	flag.StringVar(&templateFile, "template-file", templateFile, "Path to template file")
	flag.Parse()

	var err error
	tpl, err = template.New(filepath.Base(templateFile)).ParseFiles(templateFile)
	if err != nil {
		fmt.Println("Parsing template:", err)
		os.Exit(1)
	}

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

func (p project) TotalFiles() int {
	count := 0
	for _, b := range p.Builds {
		count += len(b.Build.Files)
	}
	return count
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
