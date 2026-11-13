package audit_file

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
)

const (
	pageSize             = 100
	maxPageSize          = 100
	pageReadChunk        = 64 << 10
	maxPageScanBytes     = 8 << 20
	maxPageResponseBytes = 8 << 20
	maxRecordBytes       = maxPageResponseBytes
	maxCursorLength      = 1024
)

var (
	errInvalidCursor  = errors.New("invalid audit file cursor")
	errStaleCursor    = errors.New("audit file changed since the previous page")
	errRecordTooLarge = errors.New("audit record exceeds page response limit")
)

type Page struct {
	Records    []json.RawMessage `json:"records"`
	Contexts   []RecordContext   `json:"contexts,omitempty"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

type pageCursor struct {
	Version   int    `json:"v"`
	Position  int64  `json:"p"`
	LineEnd   int64  `json:"e"`
	Size      int64  `json:"s"`
	HeadHash  string `json:"h"`
	QueryHash string `json:"q,omitempty"`
}

func readPage(path string, limit int, encodedCursor string) (*Page, error) {
	return readFilteredPage(path, limit, encodedCursor, "")
}

func readFilteredPage(path string, limit int, encodedCursor, query string) (*Page, error) {
	if limit < 1 || limit > maxPageSize {
		return nil, fmt.Errorf("%w: page size", errInvalidCursor)
	}
	query = strings.ToLower(strings.TrimSpace(query))
	if len(query) > 1024 {
		return nil, fmt.Errorf("%w: search query", errInvalidCursor)
	}
	queryHash := ""
	if query != "" {
		queryHash = fmt.Sprintf("%x", sha256.Sum256([]byte(query)))
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) && encodedCursor == "" {
		return &Page{Records: []json.RawMessage{}}, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, errStaleCursor
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("audit log is not a regular file")
	}
	page := &Page{Records: []json.RawMessage{}}
	if info.Size() == 0 {
		if encodedCursor != "" {
			return nil, errStaleCursor
		}
		return page, nil
	}

	state := pageCursor{Version: 1, Position: info.Size(), LineEnd: info.Size(), Size: info.Size()}
	if encodedCursor == "" {
		var last [1]byte
		if _, err = file.ReadAt(last[:], state.Position-1); err != nil {
			return nil, errStaleCursor
		}
		if last[0] == '\n' {
			state.Position--
			state.LineEnd--
		}
	} else {
		state, err = decodeCursor(encodedCursor)
		if err != nil {
			return nil, err
		}
		if state.QueryHash != queryHash {
			return nil, errInvalidCursor
		}
		if state.Size > info.Size() {
			return nil, errStaleCursor
		}
	}
	headHash, err := fileIdentityHash(file, path, state.Size)
	if err != nil {
		return nil, err
	}
	if encodedCursor != "" && state.HeadHash != headHash {
		return nil, errStaleCursor
	}
	state.HeadHash = headHash
	state.QueryHash = queryHash

	var responseBytes int
	var scanned int64
	for state.Position > 0 && scanned < maxPageScanBytes {
		length := min(int64(pageReadChunk), state.Position, maxPageScanBytes-scanned)
		start := state.Position - length
		block := make([]byte, length)
		if _, err = file.ReadAt(block, start); err != nil {
			return nil, errStaleCursor
		}
		for i := len(block) - 1; i >= 0; i-- {
			if block[i] != '\n' {
				continue
			}
			newline := start + int64(i)
			record, readErr := readRecord(file, newline+1, state.LineEnd)
			if readErr != nil {
				return nil, readErr
			}
			if record != nil && (query == "" || strings.Contains(strings.ToLower(string(record)), query)) {
				if responseBytes+len(record) > maxPageResponseBytes {
					state.Position = state.LineEnd
					return finishPage(page, state)
				}
				page.Records = append(page.Records, record)
				responseBytes += len(record)
			}
			state.Position = newline
			state.LineEnd = newline
			if len(page.Records) == limit {
				return finishPage(page, state)
			}
		}
		state.Position = start
		scanned += length
	}
	if state.Position == 0 {
		record, readErr := readRecord(file, 0, state.LineEnd)
		if readErr != nil {
			return nil, readErr
		}
		if record != nil && (query == "" || strings.Contains(strings.ToLower(string(record)), query)) {
			if responseBytes+len(record) > maxPageResponseBytes {
				state.Position = state.LineEnd
				return finishPage(page, state)
			}
			page.Records = append(page.Records, record)
		}
		return page, nil
	}
	return finishPage(page, state)
}

func readRecord(file *os.File, start, end int64) (json.RawMessage, error) {
	length := end - start
	if length <= 0 {
		return nil, nil
	}
	if length > maxRecordBytes {
		return nil, errRecordTooLarge
	}
	line := make([]byte, length)
	if _, err := file.ReadAt(line, start); err != nil {
		return nil, errStaleCursor
	}
	line = bytes.TrimSpace(line)
	if !json.Valid(line) {
		return nil, nil
	}
	return json.RawMessage(line), nil
}

func finishPage(page *Page, state pageCursor) (*Page, error) {
	if state.Position == 0 {
		return page, nil
	}
	data, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	return page, nil
}

func decodeCursor(encoded string) (pageCursor, error) {
	var cursor pageCursor
	if len(encoded) > maxCursorLength {
		return cursor, errInvalidCursor
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Version != 1 || cursor.Position <= 0 || cursor.LineEnd < cursor.Position || cursor.Size < cursor.LineEnd || len(cursor.HeadHash) != sha256.Size*2 {
		return pageCursor{}, errInvalidCursor
	}
	return cursor, nil
}

func fileIdentityHash(file *os.File, path string, size int64) (string, error) {
	info, err := file.Stat()
	if err != nil {
		return "", errStaleCursor
	}
	length := min(int64(pageReadChunk), size)
	buf := make([]byte, length)
	hash := sha256.New()
	_, _ = hash.Write([]byte(path + "\x00"))
	// Dev and Ino identify the opened file across renames while remaining
	// stable when new audit records are appended to that same file. Platforms
	// without these fields retain the content fingerprint below.
	if stat := reflect.ValueOf(info.Sys()); stat.IsValid() && stat.Kind() == reflect.Pointer && !stat.IsNil() {
		stat = stat.Elem()
		if stat.Kind() == reflect.Struct {
			dev, ino := stat.FieldByName("Dev"), stat.FieldByName("Ino")
			if dev.IsValid() && ino.IsValid() && dev.CanUint() && ino.CanUint() {
				_, _ = fmt.Fprintf(hash, "%d:%d\x00", dev.Uint(), ino.Uint())
			}
		}
	}
	if _, err := file.ReadAt(buf, 0); err != nil {
		return "", errStaleCursor
	}
	_, _ = hash.Write(buf)
	if _, err := file.ReadAt(buf, size-length); err != nil {
		return "", errStaleCursor
	}
	_, _ = hash.Write(buf)
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}
