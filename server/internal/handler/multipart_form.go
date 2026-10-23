package handler

import "net/http"

// parseBoundedMultipartForm caps the request body at maxBody bytes and parses
// it as multipart/form-data, keeping at most maxMemory bytes of file parts in
// memory. On success r.MultipartForm, r.Form and r.PostForm are populated
// exactly as http.Request.ParseMultipartForm would.
func parseBoundedMultipartForm(w http.ResponseWriter, r *http.Request, maxBody, maxMemory int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		return err
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return err
	}
	form, err := mr.ReadForm(maxMemory)
	if err != nil {
		return err
	}
	r.MultipartForm = form
	for k, v := range form.Value {
		r.Form[k] = append(r.Form[k], v...)
		r.PostForm[k] = append(r.PostForm[k], v...)
	}
	return nil
}

// redirectFound answers with 302 and the given absolute Location. Callers build
// the target from the configured frontend URL, so it is set as a header rather
// than passed through http.Redirect's request-relative resolution.
func redirectFound(w http.ResponseWriter, target string) {
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusFound)
}
