// SPDX-License-Identifier: AGPL-3.0-or-later

package host

import (
	"io"
	"net/http"
)

func httpHandlerFunc(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, body) })
}
