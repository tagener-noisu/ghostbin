package main

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/DHowett/ghostbin/views"
	"github.com/gorilla/mux"
	"gopkg.in/yaml.v2"
)

var base32Encoder = base32.NewEncoding("abcdefghjkmnopqrstuvwxyz23456789")

func generateRandomBytes(nbytes int) ([]byte, error) {
	uuid := make([]byte, nbytes)
	n, err := rand.Read(uuid)
	if n != len(uuid) || err != nil {
		return []byte{}, err
	}

	return uuid, nil
}

func generateRandomBase32String(nbytes, outlen int) (string, error) {
	uuid, err := generateRandomBytes(nbytes)
	if err != nil {
		return "", err
	}

	s := base32Encoder.EncodeToString(uuid)
	if outlen == -1 {
		outlen = len(s)
	}

	return s[0:outlen], nil
}

func YAMLUnmarshalFile(filename string, i interface{}) error {
	yamlFile, err := os.Open(filename)
	if err != nil {
		return err
	}

	fi, err := yamlFile.Stat()
	if err != nil {
		return err
	}

	yml := make([]byte, fi.Size())
	io.ReadFull(yamlFile, yml)
	yamlFile.Close()
	err = yaml.Unmarshal(yml, i)
	if err != nil {
		return err
	}

	return nil
}

func BaseURLForRequest(r *http.Request) *url.URL {
	determinedScheme := "http"
	if RequestIsHTTPS(r) {
		determinedScheme = "https"
	}
	return &url.URL{
		Scheme: determinedScheme,
		User:   r.URL.User,
		Host:   r.Host,
		Path:   "/",
	}
}

func RequestIsHTTPS(r *http.Request) bool {
	proto := strings.ToLower(r.URL.Scheme)
	return proto == "https"
}

func SourceIPForRequest(r *http.Request) string {
	ip := r.RemoteAddr[:strings.LastIndex(r.RemoteAddr, ":")]
	return ip
}

func HTTPSMuxMatcher(r *http.Request, rm *mux.RouteMatch) bool {
	return RequestIsHTTPS(r)
}

func NonHTTPSMuxMatcher(r *http.Request, rm *mux.RouteMatch) bool {
	return !RequestIsHTTPS(r)
}

type ByteSize float64

const (
	_           = iota // ignore first value by assigning to blank identifier
	KB ByteSize = 1 << (10 * iota)
	MB
	GB
	TB
	PB
	EB
	ZB
	YB
)

func (b ByteSize) String() string {
	switch {
	case b >= YB:
		return fmt.Sprintf("%.2fYB", b/YB)
	case b >= ZB:
		return fmt.Sprintf("%.2fZB", b/ZB)
	case b >= EB:
		return fmt.Sprintf("%.2fEB", b/EB)
	case b >= PB:
		return fmt.Sprintf("%.2fPB", b/PB)
	case b >= TB:
		return fmt.Sprintf("%.2fTB", b/TB)
	case b >= GB:
		return fmt.Sprintf("%.2fGB", b/GB)
	case b >= MB:
		return fmt.Sprintf("%.2fMB", b/MB)
	case b >= KB:
		return fmt.Sprintf("%.2fKB", b/KB)
	}
	return fmt.Sprintf("%.2fB", b)
}

func bindViews(viewModel *views.Model, dataProvider views.DataProvider, bmap map[interface{}]**views.View) error {
	var err error
	for id, vp := range bmap {
		*vp, err = viewModel.Bind(id, dataProvider)
		if err != nil {
			return err
		}
	}
	return nil
}
