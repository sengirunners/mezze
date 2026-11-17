package query

import (
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
)

// EncodeCursor preserves the work page's versioned total-order position.
func EncodeCursor(key int64, project, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("p1|" + strconv.FormatInt(key, 10) + "|" + project + "|" + id))
}

// DecodeCursor validates a position before any work source is queried.
func DecodeCursor(s string) (int64, string, string, error) {
	bad := api.Errorf(api.CodeBadRequest, "bad cursor %q", s)
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, "", "", bad
	}
	parts := strings.SplitN(string(b), "|", 4)
	if len(parts) != 4 || parts[0] != "p1" {
		return 0, "", "", bad
	}
	key, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, "", "", bad
	}
	return key, parts[2], parts[3], nil
}
