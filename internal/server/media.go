package server

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/protocol"
)

// Attachment downloads use a separate transport: no OAuth headers, environment
// proxy, redirects, or private-network destinations. DNS answers are pinned at dial.
func mediaClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, v := range ips {
			if !publicIP(v.IP) {
				return nil, errors.New("attachment URL resolves to a non-public address")
			}
		}
		var last error
		for _, v := range ips {
			conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(v.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			last = err
		}
		if last == nil {
			last = errors.New("attachment host has no IP addresses")
		}
		return nil, last
	}
	return &http.Client{Transport: t, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func publicIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	if v := ip.To4(); v != nil {
		if v[0] == 0 || v[0] == 127 || v[0] >= 224 || v[0] == 100 && v[1] >= 64 && v[1] <= 127 || v[0] == 169 && v[1] == 254 || v[0] == 198 && (v[1] == 18 || v[1] == 19) {
			return false
		}
	}
	return true
}
func (s *Server) resolveMedia(ctx context.Context, request *ag.Request) error {
	budget := s.cfg.MaxBodyBytes
	var resolve func([]ag.Part) error
	resolve = func(parts []ag.Part) error {
		for i := range parts {
			p := &parts[i]
			if p.FunctionResponse != nil {
				if err := resolve(p.FunctionResponse.Parts); err != nil {
					return err
				}
			}
			if p.FileData == nil || !strings.HasPrefix(p.FileData.URI, "https://") {
				continue
			}
			r, err := http.NewRequestWithContext(ctx, http.MethodGet, p.FileData.URI, nil)
			if err != nil {
				return err
			}
			resp, err := s.media.Do(r)
			if err != nil {
				return &protocol.Error{Param: "media", Message: "attachment download failed"}
			}
			data, readErr := io.ReadAll(io.LimitReader(resp.Body, budget+1))
			_ = resp.Body.Close()
			if readErr != nil || resp.StatusCode != 200 {
				return &protocol.Error{Param: "media", Message: "attachment download failed"}
			}
			if int64(len(data)) > budget {
				return &protocol.Error{Param: "media", Message: "attachments exceed request size limit"}
			}
			budget -= int64(len(data))
			mt := p.FileData.MIMEType
			if mt == "" {
				mt, _, _ = mime.ParseMediaType(resp.Header.Get("Content-Type"))
			}
			if mt == "" {
				mt = http.DetectContentType(data)
			}
			p.InlineData = &ag.Blob{MIMEType: mt, Data: base64.StdEncoding.EncodeToString(data)}
			p.FileData = nil
		}
		return nil
	}
	for i := range request.Contents {
		if err := resolve(request.Contents[i].Parts); err != nil {
			return err
		}
	}
	return nil
}
