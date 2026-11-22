package dav

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
)

// Resource is one multistatus entry.
type Resource struct {
	Path           string // decoded, relative to the base URL, "/"-prefixed, no trailing slash (root is "/")
	IsDir          bool
	Size           int64
	ModTime        time.Time
	ETag           string
	ContentType    string
	DisplayName    string
	QuotaUsed      int64  // -1 if not reported
	QuotaAvailable int64  // -1 if not reported
	MountType      string // Nextcloud nc:mount-type: "group" for a team folder, "shared", "external"; empty if absent
	Permissions    string // ownCloud/Nextcloud oc:permissions flags (e.g. "RMGDNVCK"); empty if absent
	FileID         string // oc:fileid: Nextcloud's numeric id, OpenCloud's composite one; empty if absent
	HasPreview     bool   // Nextcloud nc:has-preview: the server can render a thumbnail
}

const propfindBody = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns"><d:prop>
<d:resourcetype/><d:getcontentlength/><d:getlastmodified/><d:getetag/><d:getcontenttype/><d:displayname/>
<d:quota-used-bytes/><d:quota-available-bytes/><nc:mount-type/><oc:permissions/><oc:fileid/><nc:has-preview/>
</d:prop></d:propfind>`

// Propfind runs PROPFIND with Depth 0 or 1 on a path relative to the base.
func (c *Client) Propfind(ctx context.Context, rel string, depth int) ([]Resource, error) {
	return c.PropfindURL(ctx, c.URL(rel), depth)
}

// PropfindURL is Propfind for an absolute URL under the base URL's host.
func (c *Client) PropfindURL(ctx context.Context, target string, depth int) ([]Resource, error) {
	if depth != 0 && depth != 1 {
		return nil, fmt.Errorf("dav: unsupported PROPFIND depth %d", depth)
	}
	h := http.Header{}
	h.Set("Depth", strconv.Itoa(depth))
	h.Set("Content-Type", "application/xml; charset=utf-8")
	resp, err := c.Do(ctx, Request{
		Method: "PROPFIND", URL: target, Header: h, Expect: ExpectXML,
		Body: bytes.NewReader([]byte(propfindBody)), Length: int64(len(propfindBody)),
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("dav: reading PROPFIND response: %w", err)
	}
	if resp.StatusCode != http.StatusMultiStatus {
		if isHTML("", data) {
			return nil, fmt.Errorf("%w: PROPFIND %s returned an HTML page", odraz.ErrBlockedByProxy, target)
		}
		return nil, fmt.Errorf("dav: PROPFIND %s: unexpected HTTP %d (not a WebDAV server?)", target, resp.StatusCode)
	}
	res, err := ParseMultistatus(data, c.base.Path)
	if err != nil && isHTML("", data) {
		return nil, fmt.Errorf("%w: PROPFIND %s returned HTML", odraz.ErrBlockedByProxy, target)
	}
	return res, err
}

type xmlMultistatus struct {
	Responses []struct {
		Hrefs     []string `xml:"href"`
		Status    string   `xml:"status"`
		Propstats []struct {
			Status string `xml:"status"`
			Prop   struct {
				ResourceType struct {
					Collection *struct{} `xml:"collection"`
				} `xml:"resourcetype"`
				DisplayName    string `xml:"displayname"`
				ContentLength  string `xml:"getcontentlength"`
				LastModified   string `xml:"getlastmodified"`
				ETag           string `xml:"getetag"`
				ContentType    string `xml:"getcontenttype"`
				QuotaUsed      string `xml:"quota-used-bytes"`
				QuotaAvailable string `xml:"quota-available-bytes"`
				MountType      string `xml:"mount-type"`
				Permissions    string `xml:"permissions"`
				FileID         string `xml:"fileid"`
				HasPreview     string `xml:"has-preview"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// ParseMultistatus parses a 207 body. basePath is the URL path of the client
// base; hrefs are decoded and made relative to it. Entries whose properties
// were not returned with a 2xx status are skipped.
func ParseMultistatus(data []byte, basePath string) ([]Resource, error) {
	var ms xmlMultistatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		return nil, fmt.Errorf("dav: parsing multistatus: %w", err)
	}
	basePath = strings.TrimRight(basePath, "/")
	var out []Resource
	for _, r := range ms.Responses {
		if len(r.Hrefs) == 0 || (r.Status != "" && !statusOK(r.Status)) {
			continue
		}
		u, err := url.Parse(strings.TrimSpace(r.Hrefs[0]))
		if err != nil {
			return nil, fmt.Errorf("dav: bad href %q: %w", r.Hrefs[0], err)
		}
		p := u.Path // already percent-decoded, case-insensitively
		if p != basePath && !strings.HasPrefix(p, basePath+"/") {
			return nil, fmt.Errorf("dav: href %q is outside base path %q", r.Hrefs[0], basePath)
		}
		rel := strings.TrimPrefix(p, basePath)
		rel = "/" + strings.Trim(rel, "/")
		res := Resource{Path: rel, QuotaUsed: -1, QuotaAvailable: -1}
		found := false
		for _, ps := range r.Propstats {
			if !statusOK(ps.Status) {
				continue
			}
			found = true
			pr := ps.Prop
			res.IsDir = res.IsDir || pr.ResourceType.Collection != nil
			res.DisplayName = firstNonEmpty(res.DisplayName, strings.TrimSpace(pr.DisplayName))
			res.ETag = firstNonEmpty(res.ETag, strings.TrimSpace(pr.ETag))
			res.ContentType = firstNonEmpty(res.ContentType, strings.TrimSpace(pr.ContentType))
			if n, err := strconv.ParseInt(strings.TrimSpace(pr.ContentLength), 10, 64); err == nil {
				res.Size = n
			}
			if t, err := http.ParseTime(strings.TrimSpace(pr.LastModified)); err == nil {
				res.ModTime = t
			}
			res.MountType = firstNonEmpty(res.MountType, strings.TrimSpace(pr.MountType))
			res.Permissions = firstNonEmpty(res.Permissions, strings.TrimSpace(pr.Permissions))
			res.FileID = firstNonEmpty(res.FileID, strings.TrimSpace(pr.FileID))
			res.HasPreview = res.HasPreview || strings.EqualFold(strings.TrimSpace(pr.HasPreview), "true")
			if n, err := strconv.ParseInt(strings.TrimSpace(pr.QuotaUsed), 10, 64); err == nil {
				res.QuotaUsed = n
			}
			if n, err := strconv.ParseInt(strings.TrimSpace(pr.QuotaAvailable), 10, 64); err == nil {
				res.QuotaAvailable = n
			}
		}
		if found {
			out = append(out, res)
		}
	}
	return out, nil
}

// statusOK parses "HTTP/1.1 200 OK"; an empty status counts as OK.
func statusOK(s string) bool {
	f := strings.Fields(s)
	if len(f) < 2 {
		return strings.TrimSpace(s) == ""
	}
	n, err := strconv.Atoi(f[1])
	return err == nil && n >= 200 && n < 300
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
