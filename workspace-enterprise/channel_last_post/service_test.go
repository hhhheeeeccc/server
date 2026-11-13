package channel_last_post

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func TestDecodeResponseBodyRoundTrip(t *testing.T) {
	original := []byte(`[{"id":"channel-id","display_name":"Channel"}]`)

	tests := []struct {
		name   string
		encode func(*testing.T, []byte) []byte
	}{
		{name: "identity", encode: func(_ *testing.T, body []byte) []byte { return body }},
		{name: "gzip", encode: encodeGzipForTest},
		{name: "zstd", encode: encodeZstdForTest},
		{name: "br", encode: encodeBrotliForTest},
		{name: "deflate", encode: encodeDeflateForTest},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded := test.encode(t, original)
			decoded, encodeResponse, err := decodeResponseBody(test.name, encoded)
			require.NoError(t, err)
			require.Equal(t, original, decoded)

			reencoded, err := encodeResponse(decoded)
			require.NoError(t, err)
			roundTrip, _, err := decodeResponseBody(test.name, reencoded)
			require.NoError(t, err)
			require.Equal(t, original, roundTrip)
		})
	}
}

func TestDecodeResponseBodyRejectsUnsupportedEncoding(t *testing.T) {
	_, _, err := decodeResponseBody("unsupported", []byte("body"))
	require.ErrorContains(t, err, "unsupported content encoding")
}

func TestDecodeResponseBodyRejectsOversizedCompressedResponse(t *testing.T) {
	largeBody := bytes.Repeat([]byte("x"), maxDecodedResponseBytes+1)
	for _, test := range []struct {
		name   string
		encode func(*testing.T, []byte) []byte
	}{
		{name: "gzip", encode: encodeGzipForTest},
		{name: "zstd", encode: encodeZstdForTest},
		{name: "br", encode: encodeBrotliForTest},
		{name: "deflate", encode: encodeDeflateForTest},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := decodeResponseBody(test.name, test.encode(t, largeBody))
			require.ErrorContains(t, err, "decoded size limit")
		})
	}
}

func TestBufferedResponseWriterStreamsOversizedResponseUnchanged(t *testing.T) {
	dst := httptest.NewRecorder()
	buffered := newBufferedResponseWriter(dst)
	buffered.Header().Set("Content-Encoding", "gzip")
	buffered.WriteHeader(http.StatusAccepted)
	first := bytes.Repeat([]byte("a"), maxBufferedResponseBytes)
	_, err := buffered.Write(first)
	require.NoError(t, err)
	require.False(t, buffered.streaming)
	_, err = buffered.Write([]byte("bc"))
	require.NoError(t, err)
	require.True(t, buffered.streaming)
	_, err = buffered.Write([]byte("d"))
	require.NoError(t, err)
	buffered.writeTo(dst)
	require.Equal(t, http.StatusAccepted, dst.Code)
	require.Equal(t, "gzip", dst.Header().Get("Content-Encoding"))
	require.Equal(t, append(first, 'b', 'c', 'd'), dst.Body.Bytes())
}

func encodeGzipForTest(t *testing.T, body []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	_, err := writer.Write(body)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return encoded.Bytes()
}

func encodeZstdForTest(t *testing.T, body []byte) []byte {
	t.Helper()
	writer, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	defer writer.Close()
	return writer.EncodeAll(body, nil)
}

func encodeBrotliForTest(t *testing.T, body []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer := brotli.NewWriter(&encoded)
	_, err := writer.Write(body)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return encoded.Bytes()
}

func encodeDeflateForTest(t *testing.T, body []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer := zlib.NewWriter(&encoded)
	_, err := writer.Write(body)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return encoded.Bytes()
}
