// Copyright (C) 2020 Storj Labs, Inc.
// See LICENSE for copying information.

package handler

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spacemonkeygo/monkit/v3"
	"go.uber.org/zap"

	"storj.io/common/memory"
	"storj.io/common/ranger"
	"storj.io/common/ranger/httpranger"
	"storj.io/linksharing/objectmap"
	"storj.io/uplink"
	"storj.io/uplink/private/object"
)

var (
	mon = monkit.Package()
)

// Config specifies the handler configuration.
type Config struct {
	// URLBase is the base URL of the link sharing handler. It is used
	// to construct URLs returned to clients. It should be a fully formed URL.
	URLBase string

	// Templates location with html templates.
	Templates string

	// TxtRecordTTL is the duration for which an entry in the txtRecordCache is valid.
	TxtRecordTTL time.Duration
}

// Location represents geographical points
// in the globe.
type Location struct {
	Latitude  float64
	Longitude float64
}

type txtRecord struct {
	access    *uplink.Access
	root      string
	timestamp time.Time
}

type txtRecords struct {
	cache map[string]txtRecord
	ttl   time.Duration
	mu    sync.Mutex
}

// Handler implements the link sharing HTTP handler.
//
// architecture: Service
type Handler struct {
	log        *zap.Logger
	urlBase    *url.URL
	templates  *template.Template
	mapper     *objectmap.IPDB
	txtRecords *txtRecords
}

// NewHandler creates a new link sharing HTTP handler.
func NewHandler(log *zap.Logger, mapper *objectmap.IPDB, config Config) (*Handler, error) {
	urlBase, err := parseURLBase(config.URLBase)
	if err != nil {
		return nil, err
	}

	if config.Templates == "" {
		config.Templates = "./templates/*.html"
	}
	templates, err := template.ParseGlob(config.Templates)
	if err != nil {
		return nil, err
	}

	return &Handler{
		log:        log,
		urlBase:    urlBase,
		templates:  templates,
		mapper:     mapper,
		txtRecords: &txtRecords{cache: map[string]txtRecord{}, ttl: config.TxtRecordTTL},
	}, nil
}

// TODO: i could assume that it is a business logic layer, so we should remove transport and server from here.

// ServeHTTP handles link sharing requests.
func (handler *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// serveHTTP handles the request in full. the error that is returned can
	// be ignored since it was only added to facilitate monitoring.
	_ = handler.serveHTTP(w, r)
}

func (handler *Handler) serveHTTP(w http.ResponseWriter, r *http.Request) (err error) {
	ctx := r.Context()
	defer mon.Task()(&ctx)(&err)

	// separate host and port, only compare hosts
	//reqHost, _, err := net.SplitHostPort(r.Host)
	//if err != nil {
	//	return err
	//}
	serverHost, _, err := net.SplitHostPort(handler.urlBase.Host)
	if err != nil {
		return err
	}
	fmt.Println("reqHost ", r.Host)
	fmt.Println("serverHost ", serverHost)
	if r.Host != serverHost {
		return handler.handleHostingService(ctx, w, r)
	}

	locationOnly := false

	switch r.Method {
	case http.MethodHead:
		locationOnly = true
	case http.MethodGet:
	default:
		err = errors.New("method not allowed")
		http.Error(w, err.Error(), http.StatusMethodNotAllowed)
		return err
	}

	return handler.handleTraditional(ctx, w, r, locationOnly)
}

// handleTraditional deals with normal linksharing that is accessed with the URL generated by the uplink share command.
func (handler *Handler) handleTraditional(ctx context.Context, w http.ResponseWriter, r *http.Request, locationOnly bool) error {
	rawRequest, access, serializedAccess, bucket, key, err := parseRequestPath(r.URL.Path)
	if err != nil {
		err = fmt.Errorf("invalid request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return err
	}

	p, err := uplink.OpenProject(ctx, access)
	if err != nil {
		handler.handleUplinkErr(w, "open project", err)
		return err
	}
	defer func() {
		if err := p.Close(); err != nil {
			handler.log.With(zap.Error(err)).Warn("unable to close project")
		}
	}()

	if key == "" || strings.HasSuffix(key, "/") {
		if !strings.HasSuffix(r.URL.Path, "/") {
			// Call redirect because directories must have a trailing '/' for the listed hyperlinks to generate correctly.
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return nil
		}
		err = handler.servePrefix(ctx, w, p, serializedAccess, bucket, key)
		if err != nil {
			handler.handleUplinkErr(w, "list prefix", err)
		}
		return nil
	}

	o, err := p.StatObject(ctx, bucket, key)
	if err != nil {
		handler.handleUplinkErr(w, "stat object", err)
		return err
	}

	if locationOnly {
		location := makeLocation(handler.urlBase, r.URL.Path)
		http.Redirect(w, r, location, http.StatusFound)
		return nil
	}

	_, download := r.URL.Query()["download"]
	_, view := r.URL.Query()["view"]
	if !download && !view && !rawRequest {
		ipBytes, err := object.GetObjectIPs(ctx, uplink.Config{}, access, bucket, key)
		if err != nil {
			handler.handleUplinkErr(w, "get object IPs", err)
			return err
		}

		var locations []Location
		for _, ip := range ipBytes {
			info, err := handler.mapper.GetIPInfos(string(ip))
			if err != nil {
				handler.log.Error("failed to get IP info", zap.Error(err))
				continue
			}

			location := Location{
				Latitude:  info.Location.Latitude,
				Longitude: info.Location.Longitude,
			}

			locations = append(locations, location)
		}

		var input struct {
			Name      string
			Size      string
			Locations []Location
			Pieces    int64
		}
		input.Name = o.Key
		input.Size = memory.Size(o.System.ContentLength).Base10String()
		input.Locations = locations
		input.Pieces = int64(len(locations))

		return handler.templates.ExecuteTemplate(w, "single-object.html", input)
	}

	if download {
		segments := strings.Split(key, "/")
		obj := segments[len(segments)-1]
		w.Header().Set("Content-Disposition", "attachment; filename=\""+obj+"\"")
	}
	httpranger.ServeContent(ctx, w, r, key, o.System.Created, newObjectRanger(p, o, bucket))
	return nil
}

func (handler *Handler) servePrefix(ctx context.Context, w http.ResponseWriter, project *uplink.Project, serializedAccess string, bucket, prefix string) (err error) {
	type Item struct {
		Name   string
		Size   string
		Prefix bool
	}

	type Breadcrumb struct {
		Prefix string
		URL    string
	}

	var input struct {
		Bucket      string
		Breadcrumbs []Breadcrumb
		Items       []Item
	}
	input.Bucket = bucket
	input.Breadcrumbs = append(input.Breadcrumbs, Breadcrumb{
		Prefix: bucket,
		URL:    serializedAccess + "/" + bucket + "/",
	})
	if prefix != "" {
		trimmed := strings.TrimRight(prefix, "/")
		for i, prefix := range strings.Split(trimmed, "/") {
			input.Breadcrumbs = append(input.Breadcrumbs, Breadcrumb{
				Prefix: prefix,
				URL:    input.Breadcrumbs[i].URL + "/" + prefix + "/",
			})
		}
	}

	input.Items = make([]Item, 0)

	objects := project.ListObjects(ctx, bucket, &uplink.ListObjectsOptions{
		Prefix: prefix,
		System: true,
	})

	// TODO add paging
	for objects.Next() {
		item := objects.Item()
		name := item.Key[len(prefix):]
		input.Items = append(input.Items, Item{
			Name:   name,
			Size:   memory.Size(item.System.ContentLength).Base10String(),
			Prefix: item.IsPrefix,
		})
	}
	if objects.Err() != nil {
		return objects.Err()
	}

	return handler.templates.ExecuteTemplate(w, "prefix-listing.html", input)
}

func (handler *Handler) handleUplinkErr(w http.ResponseWriter, action string, err error) {
	switch {
	case errors.Is(err, uplink.ErrBucketNotFound):
		w.WriteHeader(http.StatusNotFound)
		err = handler.templates.ExecuteTemplate(w, "404.html", "Oops! Bucket not found.")
		if err != nil {
			handler.log.Error("error while executing template", zap.Error(err))
		}
	case errors.Is(err, uplink.ErrObjectNotFound):
		w.WriteHeader(http.StatusNotFound)
		err = handler.templates.ExecuteTemplate(w, "404.html", "Oops! Object not found.")
		if err != nil {
			handler.log.Error("error while executing template", zap.Error(err))
		}
	default:
		handler.log.Error("unable to handle request", zap.String("action", action), zap.Error(err))
		http.Error(w, "unable to handle request", http.StatusInternalServerError)
	}
}

func parseRequestPath(p string) (rawRequest bool, _ *uplink.Access, serializedAccess, bucket, key string, err error) {
	// Drop the leading slash, if necessary.
	p = strings.TrimPrefix(p, "/")

	// Split the request path.
	segments := strings.SplitN(p, "/", 4)
	if len(segments) == 4 {
		if segments[0] == "raw" {
			rawRequest = true
			segments = segments[1:]
		} else {
			// if its not a raw request, we need to concat the last two entries as those contain paths in the bucket
			// and shrink the array again
			rawRequest = false
			segments[2] = segments[2] + "/" + segments[3]
			segments = segments[:len(segments)-1]
		}

	}
	if len(segments) == 1 {
		if segments[0] == "" {
			return rawRequest, nil, "", "", "", errors.New("missing access")
		}
		return rawRequest, nil, "", "", "", errors.New("missing bucket")
	}

	serializedAccess = segments[0]
	bucket = segments[1]

	if len(segments) == 3 {
		key = segments[2]
	}

	access, err := uplink.ParseAccess(serializedAccess)
	if err != nil {
		return rawRequest, nil, "", "", "", err
	}
	return rawRequest, access, serializedAccess, bucket, key, nil
}

type objectRanger struct {
	p      *uplink.Project
	o      *uplink.Object
	bucket string
}

func newObjectRanger(p *uplink.Project, o *uplink.Object, bucket string) ranger.Ranger {
	return &objectRanger{
		p:      p,
		o:      o,
		bucket: bucket,
	}
}

func (ranger *objectRanger) Size() int64 {
	return ranger.o.System.ContentLength
}

func (ranger *objectRanger) Range(ctx context.Context, offset, length int64) (_ io.ReadCloser, err error) {
	defer mon.Task()(&ctx)(&err)
	return ranger.p.DownloadObject(ctx, ranger.bucket, ranger.o.Key, &uplink.DownloadOptions{Offset: offset, Length: length})
}

func parseURLBase(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}

	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, errors.New("URL base must be http:// or https://")
	case u.Host == "":
		return nil, errors.New("URL base must contain host")
	case u.User != nil:
		return nil, errors.New("URL base must not contain user info")
	case u.RawQuery != "":
		return nil, errors.New("URL base must not contain query values")
	case u.Fragment != "":
		return nil, errors.New("URL base must not contain a fragment")
	}
	return u, nil
}

func makeLocation(base *url.URL, reqPath string) string {
	location := *base
	location.Path = path.Join(location.Path, reqPath)
	return location.String()
}

// handleHostingService deals with linksharing via custom URLs.
func (handler *Handler) handleHostingService(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		handler.log.Error("unable to handle request", zap.Error(err))
		http.Error(w, "unable to handle request", http.StatusInternalServerError)
		return err
	}

	access, root, err := handler.getRootAndAccess(host)
	if err != nil {
		handler.log.Error("unable to handle request", zap.Error(err))
		http.Error(w, "unable to handle request", http.StatusInternalServerError)
		return err
	}

	project, err := uplink.OpenProject(ctx, access)
	if err != nil {
		handler.handleUplinkErr(w, "open project", err)
		return err
	}
	defer func() {
		if err := project.Close(); err != nil {
			handler.log.With(zap.Error(err)).Warn("unable to close project")
		}
	}()

	// e.g. http://mydomain.com/folder2/index.html with root="bucket1/folder1"
	rootPath := strings.SplitN(root, "/", 2) // e.g. rootPath=[bucket1, folder1]
	bucket := rootPath[0]
	pt := strings.TrimPrefix(r.URL.Path, "/") // e.g. path = "folder2/index.html
	if len(rootPath) == 2 {
		pathPrefix := rootPath[1]  // e.g. pathPrefix = "folder1"
		pt = pathPrefix + "/" + pt // e.g. path="folder1/folder2/index.html"
	}

	if pt == "" {
		pt = "index.html"
	}

	o, err := project.StatObject(ctx, bucket, pt)
	if err != nil {
		handler.handleUplinkErr(w, "stat object", err)
		return err
	}

	httpranger.ServeContent(ctx, w, r, pt, o.System.Created, newObjectRanger(project, o, bucket))
	return nil
}

// getRootAndAccess fetches the root and access grant from the cache or dns server when applicable.
func (handler *Handler) getRootAndAccess(hostname string) (access *uplink.Access, root string, err error) {
	record, exists := handler.checkCache(hostname)
	if exists {
		return record.access, record.root, nil
	}
	access, root, err = getRemoteRecord(hostname)
	if err != nil {
		return access, root, err
	}
	handler.updateCache(hostname, root, access)

	return access, root, err
}

// checkCache checks the txt record cache to see if we have a valid access grant and root path.
func (handler *Handler) checkCache(hostname string) (record txtRecord, exists bool) {
	handler.txtRecords.mu.Lock()
	defer handler.txtRecords.mu.Unlock()

	record, ok := handler.txtRecords.cache[hostname]
	if ok && !recordIsExpired(record, handler.txtRecords.ttl) {
		return record, true
	}
	return record, false
}

// recordIsExpired checks whether an entry in the txtRecord cache is expired.
// A record is expired if its last timestamp plus the ttl was in the past.
func recordIsExpired(record txtRecord, ttl time.Duration) bool {
	return record.timestamp.Add(ttl).Before(time.Now())
}

// getRemoteRecord does an txt record lookup for the hostname on the dns server.
func getRemoteRecord(hostname string) (access *uplink.Access, root string, err error) {
	records, err := net.LookupTXT(hostname)
	if err != nil {
		return access, root, err
	}
	return parseRecords(records)
}

// parseRecords transforms the data from the hostname's external TXT records.
// For example, a hostname may have the following TXT records: "storj_grant-1:abcd", "storj_grant-2:efgh", "storj_root:mybucket/folder".
// parseRecords then will return serializedAccess="abcdefgh" and root="mybucket/folder".
func parseRecords(records []string) (access *uplink.Access, root string, err error) {
	grants := map[int]string{}
	for _, record := range records {
		r := strings.SplitN(record, ":", 2)
		if strings.HasPrefix(r[0], "storj_grant") {
			section := strings.Split(r[0], "-")
			key, err := strconv.Atoi(section[1])
			if err != nil {
				return access, root, err
			}
			grants[key] = r[1]
		} else if r[0] == "storj_root" {
			root = r[1]
		}
	}

	if root == "" {
		return access, root, errors.New("missing root path in txt record")
	}

	var serializedAccess string
	for i := 1; i <= len(grants); i++ {
		if grants[i] == "" {
			return access, root, errors.New("missing grants")
		}
		serializedAccess += grants[i]
	}
	access, err = uplink.ParseAccess(serializedAccess)
	return access, root, err
}

// updateCache updates the txtRecord cache with the hostname and corresponding access, root, and time of update.
func (handler *Handler) updateCache(hostname, root string, access *uplink.Access) {
	handler.txtRecords.mu.Lock()
	defer handler.txtRecords.mu.Unlock()

	handler.txtRecords.cache[hostname] = txtRecord{access: access, root: root, timestamp: time.Now()}
}
