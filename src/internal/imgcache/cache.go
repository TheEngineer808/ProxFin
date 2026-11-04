package imgcache

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Limits for downloading one image. Without a timeout, one image host that
// accepted the connection and never answered stalled the whole cache.
var (
	downloadTimeout       = 30 * time.Second
	maxImageSize    int64 = 32 << 20
)

// Cache : Cache strcut
type Cache struct {
	path     string
	cacheURL string
	caching  bool
	images   map[string]string
	Queue    []string
	queued   map[string]bool // the URLs in Queue
	cached   map[string]bool // file names in the cache folder
	busy     bool            // Caching is running
	client   *http.Client
	Image    imageFunc
	sync.RWMutex
}

type imageFunc struct {
	GetURL  func(string, string, string, bool, int, string) string
	Caching func()
	Remove  func()
}

// New : New cahce
func New(path, cacheURL string, caching bool) (c *Cache, err error) {

	c = &Cache{}

	c.images = make(map[string]string)
	c.path = path
	c.cacheURL = cacheURL
	c.caching = caching
	c.Queue = []string{}
	c.queued = make(map[string]bool)
	c.cached = make(map[string]bool)
	c.client = &http.Client{Timeout: downloadTimeout}

	c.Image.GetURL = func(src string, http_domain string, http_port string, force_https bool, https_port int, https_domain string) (cacheURL string) {

		c.Lock()
		defer c.Unlock()

		src = strings.Trim(src, "\r\n")

		if !c.caching {
			return src
		}

		u, err := url.Parse(src)

		if err != nil || len(filepath.Ext(u.Path)) == 0 {
			return src
		}

		src_filtered := strings.Split(src, "?")
		var filename = fmt.Sprintf("%s%s", strToMD5(src_filtered[0]), filepath.Ext(u.Path))

		if cacheURL, ok := c.images[filename]; ok {
			if c.caching && force_https {
				u, err := url.Parse(cacheURL)
				if err == nil {
					cacheURL = fmt.Sprintf("https://%s:%d%s", https_domain, https_port, u.Path)
				}
			} else if c.caching && http_domain != "" {
				u, err := url.Parse(cacheURL)
				if err == nil {
					var baseUrl = ""
					if strings.Contains(http_domain, ":") {
						baseUrl = http_domain
					} else {
						baseUrl = fmt.Sprintf("%s:%s", http_domain, http_port)
					}
					cacheURL = fmt.Sprintf("http://%s%s", baseUrl, u.Path)
				}
			}
			return cacheURL
		}

		if !c.cached[filename] {
			if !c.queued[src] {
				c.queued[src] = true
				c.Queue = append(c.Queue, src)
			}

		} else {
			c.images[filename] = c.cacheURL + filename
			src = c.cacheURL + filename
		}

		return src
	}

	// Caching downloads the queued images. The downloads run without
	// holding the lock, so GetURL (used while building the M3U and XMLTV
	// files) is never blocked by a slow image host.
	c.Image.Caching = func() {

		c.Lock()
		if c.busy {
			c.Unlock()
			return
		}
		c.busy = true
		var queue = append([]string(nil), c.Queue...)
		c.Unlock()

		defer func() {
			c.Lock()
			// Keep what is still queued (failed downloads, and images
			// queued by GetURL in the meantime).
			var remaining = c.Queue[:0]
			for _, src := range c.Queue {
				if c.queued[src] {
					remaining = append(remaining, src)
				}
			}
			c.Queue = remaining
			c.busy = false
			c.Unlock()
		}()

		for _, src := range queue {

			src_filtered := strings.Split(src, "?")
			u, err := url.Parse(src_filtered[0])
			if err != nil {
				continue
			}

			var name = fmt.Sprintf("%s%s", strToMD5(src_filtered[0]), filepath.Ext(u.Path))

			if err := c.download(src, c.path+name); err != nil {
				continue
			}

			c.Lock()
			c.cached[name] = true
			c.images[name] = c.cacheURL + name
			delete(c.queued, src)
			c.Unlock()
		}

	}

	c.Image.Remove = func() {

		c.Lock()
		defer c.Unlock()

		// Images are being downloaded: they aren't in c.images yet.
		if c.busy {
			return
		}

		files, err := os.ReadDir(c.path)
		if err != nil {
			return
		}

		for _, file := range files {

			switch c.caching {

			case true:
				if _, ok := c.images[file.Name()]; !ok {
					os.RemoveAll(c.path + file.Name())
				}

			case false:
				os.RemoveAll(c.path + file.Name())
			}

		}

	}

	files, err := os.ReadDir(c.path)
	if err != nil {
		return
	}

	for _, file := range files {
		c.cached[file.Name()] = true
	}

	return
}

// download saves the image at src to filename. It is written to a
// temporary file first, so a failed download never leaves a partial image
// in the cache.
func (c *Cache) download(src, filename string) (err error) {

	resp, err := c.client.Get(src)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", src, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(filename), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename

	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxImageSize+1))
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if n > maxImageSize {
		return fmt.Errorf("%s: image larger than %d bytes", src, maxImageSize)
	}

	return os.Rename(tmp.Name(), filename)
}
