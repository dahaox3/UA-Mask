package main

import (
	"bufio"
	"errors"
	"io"
	"net/http"
)

const defaultMaxHeaderBytes = 64 * 1024
const maxHeaderFields = 256

var errHeaderTooLarge = errors.New("HTTP request headers exceed configured limit")

// Only limit reads while parsing headers. Reuse the same bufio.Reader for
// streamed bodies and pipelined requests; never discard prefetched bytes.
type headerBudgetReader struct {
	r               io.Reader
	limited         bool
	remaining, read int
}

func (r *headerBudgetReader) Read(p []byte) (int, error) {
	if r.limited {
		if r.remaining <= 0 {
			return 0, errHeaderTooLarge
		}
		if len(p) > r.remaining {
			p = p[:r.remaining]
		}
	}
	n, err := r.r.Read(p)
	if r.limited {
		r.remaining -= n
		r.read += n
	}
	return n, err
}

func readBoundedRequest(reader *bufio.Reader, budget *headerBudgetReader, limit int) (*http.Request, error) {
	buffered := reader.Buffered()
	budget.limited, budget.remaining, budget.read = true, limit-buffered, 0
	req, err := http.ReadRequest(reader)
	budget.limited = false
	if err != nil {
		if budget.remaining <= 0 {
			return nil, errHeaderTooLarge
		}
		return nil, err
	}
	consumed := buffered + budget.read - reader.Buffered()
	fields := 1 // Host is removed from Header by net/http.
	for _, values := range req.Header {
		fields += len(values)
	}
	if consumed > limit || fields > maxHeaderFields {
		// The owner closes the connection on error. Do not drain an untrusted
		// body here: Close on net/http's server body can perform blocking reads.
		return nil, errHeaderTooLarge
	}
	return req, nil
}
