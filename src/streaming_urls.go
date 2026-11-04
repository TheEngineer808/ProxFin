package src

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"sync"
)

// The streaming URL cache (Data.Cache.StreamingURLS) maps the IDs in
// ProxFin's /stream/ URLs to the provider URLs. Channel tunes read it
// while playlist and EPG updates rebuild it, so all access goes through
// these functions.
//
// A rebuild used to empty the cache and fill it again only when the M3U
// was generated at its end, so every channel tuned during an update (which
// can take minutes on large playlists) failed with a 404. Now the cache is
// never emptied: a reset starts a new generation, and only entries that
// were not handed out (stored) in the last few generations are removed.
// A rebuild resets twice, so an entry survives at least one full rebuild
// in which its channel no longer appears.
//
// After a start, urls.json is read into the cache once, when it is first
// used. The rebuild that runs at startup resets and saves the cache before
// it generates the M3U in the background, while clients already tune; it
// used to save the not yet loaded (empty) cache over urls.json, so every
// channel failed with a 404 until the M3U was ready.

const streamingURLKeepGenerations = 3

var (
	streamingURLsMu  sync.RWMutex
	streamingURLsGen uint64            // incremented by every reset
	streamingURLsAge map[string]uint64 // generation each entry was last stored in

	// Serialises writes of urls.json, so that a slower save of an older
	// snapshot cannot overwrite a newer one.
	streamingURLsSaveMu sync.Mutex
)

// resetStreamingURLs starts a new generation and removes entries that were
// not stored in the last streamingURLKeepGenerations generations.
func resetStreamingURLs() {
	streamingURLsMu.Lock()
	defer streamingURLsMu.Unlock()

	loadStreamingURLsLocked()
	streamingURLsGen++
	for id, gen := range streamingURLsAge {
		if streamingURLsGen-gen > streamingURLKeepGenerations {
			delete(Data.Cache.StreamingURLS, id)
			delete(streamingURLsAge, id)
		}
	}
}

// saveStreamingURLs writes the cache to urls.json. The file is written
// from a copy: holding the lock during the write made every tune wait for
// the disk whenever a rebuild was waiting to store an entry.
func saveStreamingURLs() {
	streamingURLsSaveMu.Lock()
	defer streamingURLsSaveMu.Unlock()

	streamingURLsMu.RLock()
	urls := maps.Clone(Data.Cache.StreamingURLS)
	streamingURLsMu.RUnlock()

	if urls == nil {
		return // not used since the start: urls.json is up to date
	}
	saveMapToJSONFile(System.File.URLS, urls)
}

// storeStreamingURL returns the entry for id in the current generation,
// storing info if the entry is missing or from an earlier generation (so a
// changed channel, e.g. new backup channels, is picked up by the next
// rebuild, as before).
func storeStreamingURL(id string, info StreamInfo) StreamInfo {
	streamingURLsMu.Lock()
	defer streamingURLsMu.Unlock()

	loadStreamingURLsLocked()
	if s, ok := Data.Cache.StreamingURLS[id]; ok && streamingURLsAge[id] == streamingURLsGen {
		return s
	}
	Data.Cache.StreamingURLS[id] = info
	streamingURLsAge[id] = streamingURLsGen
	return info
}

// lookupStreamingURL finds id, loading the cache from urls.json first if it
// has not been used since the start. That happens once: the cache used to
// be reloaded on every miss while it was empty (a new install, or no
// channels), so each request for an unknown stream read and parsed the
// file under the exclusive lock.
func lookupStreamingURL(id string) (StreamInfo, bool, error) {
	streamingURLsMu.RLock()
	s, ok := Data.Cache.StreamingURLS[id]
	loaded := Data.Cache.StreamingURLS != nil
	streamingURLsMu.RUnlock()

	if ok || loaded {
		return s, ok, nil
	}

	streamingURLsMu.Lock()
	defer streamingURLsMu.Unlock()

	err := loadStreamingURLsLocked()
	s, ok = Data.Cache.StreamingURLS[id]
	return s, ok, err
}

// loadStreamingURLsLocked reads urls.json into the cache if the cache has
// not been used since the start (whatever the outcome, the file is not
// read again). It needs streamingURLsMu held for writing.
func loadStreamingURLsLocked() error {
	if Data.Cache.StreamingURLS != nil {
		return nil
	}
	ensureStreamingURLMaps()

	tmp, err := loadJSONFileToMap(System.File.URLS)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // nothing saved yet
	}
	if err != nil {
		return err
	}
	urls := make(map[string]StreamInfo)
	if err := json.Unmarshal([]byte(mapToJSON(tmp)), &urls); err != nil {
		return err
	}
	for k, v := range urls {
		Data.Cache.StreamingURLS[k] = v
		streamingURLsAge[k] = streamingURLsGen
	}
	// The saved entries count as from the previous generation: the
	// channels stored from now on replace them with their current details.
	streamingURLsGen++
	return nil
}

// ensureStreamingURLMaps needs streamingURLsMu held for writing.
func ensureStreamingURLMaps() {
	if Data.Cache.StreamingURLS == nil {
		Data.Cache.StreamingURLS = make(map[string]StreamInfo)
	}
	if streamingURLsAge == nil {
		streamingURLsAge = make(map[string]uint64)
	}
}
