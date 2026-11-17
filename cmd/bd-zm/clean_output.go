package main

import (
	"github.com/sengirunners/potluck/kit/api/textclean"
	"io"
)

type cleanWriter struct{ io.Writer }

func (w cleanWriter) Write(p []byte) (int, error) {
	_, err := io.WriteString(w.Writer, textclean.Clean(string(p)))
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
