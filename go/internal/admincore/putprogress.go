package admincore

// progressPutter is an optional capability of a Store: an upload that reports
// how far it has got.
//
// It is discovered by assertion rather than added to Store, because Store is
// implemented by every test fake in this repository and by the agent's own
// client, none of which have any use for it. A store without it simply uploads
// without reporting, which is what the terminal-less paths want anyway.
type progressPutter interface {
	PutProgress(key string, data []byte, onProgress func(done, total int64)) error
}

// putReporting uploads, reporting progress when the store can.
//
// onProgress may be nil, and the total it reports is the payload size, so a
// caller can render a bar without knowing which path was taken.
func putReporting(s Store, key string, data []byte, onProgress func(done, total int64)) error {
	if onProgress == nil {
		return s.Put(key, data)
	}
	if pp, ok := s.(progressPutter); ok {
		return pp.PutProgress(key, data, onProgress)
	}
	// No reporting available: say it is starting, upload, say it is done, so
	// the caller's bar still ends where it should rather than stopping short.
	onProgress(0, int64(len(data)))
	if err := s.Put(key, data); err != nil {
		return err
	}
	onProgress(int64(len(data)), int64(len(data)))
	return nil
}
