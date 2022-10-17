package fetcher

import (
	"compress/gzip"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/guonaihong/gout"
	"github.com/zzzhr1990/simple-updater/model"
)

type Interface interface {
	GetUpdateInfo(channel string, name string, currentVersion string) (*model.UpdateCollection, error)
	Init() error
	GetUpdateStream(url string) (io.Reader, error)
}

/*
import "io"

// Interface defines the required fetcher functions
type Interface interface {
	//Init should perform validation on fields. For
	//example, ensure the appropriate URLs or keys
	//are defined or ensure there is connectivity
	//to the appropriate web service.
	//Init() error
	//Fetch should check if there is an updated
	//binary to fetch, and then stream it back the
	//form of an io.Reader. If io.Reader is nil,
	//then it is assumed there are no updates. Fetch
	//will be run repeatedly and forever. It is up the
	//implementation to throttle the fetch frequency.
	//Fetch() (io.Reader, error)
}

// Func converts a fetch function into the fetcher interface
func Func(fn func() (io.Reader, error)) Interface {
	return &fetcher{fn}
}

type fetcher struct {
	fn func() (io.Reader, error)
}

func (f fetcher) Init() error {
	return nil //skip
}

func (f fetcher) Fetch() (io.Reader, error) {
	return f.fn()
}
*/

type SimpleFetcher struct {
	//fn func() (io.Reader, error)
	first bool
}

func (f SimpleFetcher) GetUpdateInfo(channel string, name string, currentVersion string) (*model.UpdateCollection, error) {
	mc := &model.UpdateCollection{}
	currentPlatform := runtime.GOOS + "-" + runtime.GOARCH
	updateInfoPrefix := "https://updates.2dland.cn/simple"
	updateInfoURL := fmt.Sprintf("%s/%s/%s/%s/latest.json", updateInfoPrefix, name, currentPlatform, channel)
	goutReq := gout.NewWithOpt(gout.WithInsecureSkipVerify(), gout.WithTimeout(time.Minute))
	if f.first {
		fmt.Println("get update info from:", updateInfoURL)
		f.first = false
	}

	err := goutReq.GET(updateInfoURL).SetQuery(gout.H{
		"platform": currentPlatform,
		"program":  name,
		"version":  currentVersion,
		"channel":  channel,
	}).BindJSON(mc).Do()
	if err != nil {
		return nil, err
	}
	return mc, nil
}
func (f SimpleFetcher) Init() error {
	return nil //skip
}
func (f SimpleFetcher) GetUpdateStream(url string) (io.Reader, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr, Timeout: time.Minute * 5}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET request failed (%s)", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET request failed (status code %d)", resp.StatusCode)
	}
	//extract gz files
	if strings.HasSuffix(url, ".gz") && resp.Header.Get("Content-Encoding") != "gzip" {
		return gzip.NewReader(resp.Body)
	}
	//success!
	return resp.Body, nil
}
