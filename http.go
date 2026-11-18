// http.go — a small raw TCP HTTP/1.1 server, built for the timed workloads.
//
// No net/http: the handler parses the request line and headers itself, reads
// the body (Content-Length or chunked), dispatches, and writes the response
// on the same keep-alive connection. One goroutine per connection; the store
// dispatch is serialized by a mutex (the workloads are sequential except
// clone, and a cached clone pack is a few hundred bytes to write).
package main

import (
	"bufio"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// Request is one parsed HTTP request. Header names are lower-cased.
type Request struct {
	Method  string
	Path    string // decoded path (no query)
	RawPath string
	Query   string // raw query string, no leading '?'
	Headers map[string]string
	Body    []byte
	Close   bool // close the connection after this response
}

func (r *Request) header(name string) string {
	return r.Headers[strings.ToLower(name)]
}

// Response is what a handler returns. Extra holds additional headers
// (X-GitHub-Request-Id, Link, X-OAuth-Scopes, ...).
type Response struct {
	Status      int
	ContentType string
	Body        []byte
	Extra       map[string]string
	NoBody      bool // write headers only (204)
}

// Server owns the store and serializes dispatch.
type Server struct {
	store *Store
	mu    sync.Mutex
}

func newServer(store *Store) *Server { return &Server{store: store} }

func (s *Server) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 65536)
	bw := bufio.NewWriterSize(conn, 65536)
	for {
		req, err := readRequest(br, bw)
		if err != nil {
			return
		}
		s.mu.Lock()
		resp := dispatch(s.store, req)
		s.mu.Unlock()
		if err := writeResponse(bw, req, resp); err != nil {
			return
		}
		if err := bw.Flush(); err != nil {
			return
		}
		if req.Close {
			return
		}
	}
}

// readRequest parses one request off the buffered connection. bw is used to
// answer `Expect: 100-continue` before the body is read.
func readRequest(br *bufio.Reader, bw *bufio.Writer) (*Request, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, io.ErrUnexpectedEOF
	}
	method, target := parts[0], parts[1]
	// Absolute-form target: strip scheme://host.
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		if idx := strings.Index(target[8:], "/"); idx >= 0 {
			target = target[8+idx:]
		} else {
			target = "/"
		}
	}
	path, query := target, ""
	if idx := strings.Index(target, "?"); idx >= 0 {
		path, query = target[:idx], target[idx+1:]
	}
	decoded, derr := url.PathUnescape(path)
	if derr == nil {
		path = decoded
	}
	req := &Request{
		Method:  method,
		Path:    path,
		RawPath: target,
		Query:   query,
		Headers: make(map[string]string, 8),
	}
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		h = strings.TrimRight(h, "\r\n")
		if h == "" {
			break
		}
		colon := strings.IndexByte(h, ':')
		if colon < 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(h[:colon]))
		value := strings.TrimSpace(h[colon+1:])
		req.Headers[name] = value
	}
	// HTTP/1.0 defaults to close; HTTP/1.1 defaults to keep-alive.
	if strings.EqualFold(req.Headers["connection"], "close") {
		req.Close = true
	}
	// 100-continue: answer before reading the body.
	if strings.EqualFold(req.Headers["expect"], "100-continue") {
		if _, err := bw.WriteString("HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
			return nil, err
		}
		if err := bw.Flush(); err != nil {
			return nil, err
		}
	}
	// Body: Content-Length, else chunked, else none.
	if cl := req.Headers["content-length"]; cl != "" {
		n, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || n < 0 {
			return nil, io.ErrUnexpectedEOF
		}
		if n > 0 {
			body := make([]byte, n)
			if _, err := io.ReadFull(br, body); err != nil {
				return nil, err
			}
			req.Body = body
		}
	} else if strings.Contains(strings.ToLower(req.Headers["transfer-encoding"]), "chunked") {
		body, err := readChunked(br)
		if err != nil {
			return nil, err
		}
		req.Body = body
	}
	return req, nil
}

func readChunked(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		sizeLine, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sizeLine = strings.TrimSpace(sizeLine)
		if idx := strings.IndexByte(sizeLine, ';'); idx >= 0 {
			sizeLine = sizeLine[:idx]
		}
		n, err := strconv.ParseInt(sizeLine, 16, 64)
		if err != nil || n < 0 {
			return nil, io.ErrUnexpectedEOF
		}
		if n == 0 {
			// Trailer headers until an empty line.
			for {
				t, err := br.ReadString('\n')
				if err != nil {
					return nil, err
				}
				if strings.TrimRight(t, "\r\n") == "" {
					break
				}
			}
			return out, nil
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		// Trailing CRLF after the chunk.
		if _, err := br.ReadString('\n'); err != nil {
			return nil, err
		}
	}
}

var reasonPhrases = map[int]string{
	200: "OK", 201: "Created", 202: "Accepted", 204: "No Content",
	400: "Bad Request", 401: "Unauthorized", 404: "Not Found",
	405: "Method Not Allowed", 409: "Conflict", 422: "Unprocessable Entity",
	500: "Internal Server Error",
}

func writeResponse(bw *bufio.Writer, req *Request, resp *Response) error {
	reason, ok := reasonPhrases[resp.Status]
	if !ok {
		reason = "OK"
	}
	var sb strings.Builder
	sb.WriteString("HTTP/1.1 ")
	sb.WriteString(strconv.Itoa(resp.Status))
	sb.WriteByte(' ')
	sb.WriteString(reason)
	sb.WriteString("\r\n")
	if resp.ContentType != "" {
		sb.WriteString("Content-Type: ")
		sb.WriteString(resp.ContentType)
		sb.WriteString("\r\n")
	}
	sb.WriteString("Content-Length: ")
	if resp.NoBody {
		sb.WriteString("0\r\n")
	} else {
		sb.WriteString(strconv.Itoa(len(resp.Body)))
		sb.WriteString("\r\n")
	}
	for k, v := range resp.Extra {
		sb.WriteString(k)
		sb.WriteString(": ")
		sb.WriteString(v)
		sb.WriteString("\r\n")
	}
	if req.Close {
		sb.WriteString("Connection: close\r\n")
	} else {
		sb.WriteString("Connection: keep-alive\r\n")
	}
	sb.WriteString("\r\n")
	if _, err := bw.WriteString(sb.String()); err != nil {
		return err
	}
	if !resp.NoBody && len(resp.Body) > 0 {
		if _, err := bw.Write(resp.Body); err != nil {
			return err
		}
	}
	return nil
}
