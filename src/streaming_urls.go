package src

import (
	"encoding/json"
	"sync"
)

// The streaming URL cache (Data.Cache.StreamingURLS) maps the IDs in
// Threadfin's /stream/ URLs to the provider URLs. Channel tunes read it
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

const streamingURLKeepGenerations = 3

var (
	streamingURLsMu  sync.RWMutex
	streamingURLsGen uint64            // incremented by every reset
	streamingURLsAge map[string]uint64 // generation each entry was last stored in
)

// resetStreamingURLs starts a new generation and removes entries that were
// not stored in the last streamingURLKeepGenerations generations.
func resetStreamingURLs() {
	streamingURLsMu.Lock()
	defer streamingURLsMu.Unlock()

	streamingURLsGen++
	for id, gen := range streamingURLsAge {
		if streamingURLsGen-gen > streamingURLKeepGenerations {
			delete(Data.Cache.StreamingURLS, id)
			delete(streamingURLsAge, id)
		}
	}
}

// saveStreamingURLs writes the cache to urls.json.
func saveStreamingURLs() {
	streamingURLsMu.RLock()
	defer streamingURLsMu.RUnlock()

	saveMapToJSONFile(System.File.URLS, Data.Cache.StreamingURLS)
}

// storeStreamingURL returns the entry for id in the current generation,
// storing info if the entry is missing or from an earlier generation (so a
// changed channel, e.g. new backup channels, is picked up by the next
// rebuild, as before).
func storeStreamingURL(id string, info StreamInfo) StreamInfo {
	streamingURLsMu.Lock()
	defer streamingURLsMu.Unlock()

	ensureStreamingURLMaps()
	if s, ok := Data.Cache.StreamingURLS[id]; ok && streamingURLsAge[id] == streamingURLsGen {
		return s
	}
	Data.Cache.StreamingURLS[id] = info
	streamingURLsAge[id] = streamingURLsGen
	return info
}

// lookupStreamingURL finds id. After a restart the cache is empty and is
// loaded from urls.json first.
func lookupStreamingURL(id string) (StreamInfo, bool, error) {
	streamingURLsMu.RLock()
	s, ok := Data.Cache.StreamingURLS[id]
	empty := len(Data.Cache.StreamingURLS) == 0
	streamingURLsMu.RUnlock()

	if ok || !empty {
		return s, ok, nil
	}

	streamingURLsMu.Lock()
	defer streamingURLsMu.Unlock()

	if len(Data.Cache.StreamingURLS) == 0 {
		tmp, err := loadJSONFileToMap(System.File.URLS)
		if err != nil {
			return StreamInfo{}, false, err
		}
		urls := make(map[string]StreamInfo)
		if err := json.Unmarshal([]byte(mapToJSON(tmp)), &urls); err != nil {
			return StreamInfo{}, false, err
		}
		ensureStreamingURLMaps()
		for k, v := range urls {
			Data.Cache.StreamingURLS[k] = v
			streamingURLsAge[k] = streamingURLsGen
		}
	}

	s, ok = Data.Cache.StreamingURLS[id]
	return s, ok, nil
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
