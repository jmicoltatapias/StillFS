package s3api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"

	"github.com/jmicoltatapias/stillfs/internal/sigv4"
	"github.com/jmicoltatapias/stillfs/internal/storage"
)

type requestAPIError struct {
	Status  int
	Code    string
	Message string
}

func (e *requestAPIError) Error() string { return e.Code + ": " + e.Message }

type errorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId"`
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *requestAPIError
	if errors.As(err, &apiErr) {
		writeS3Error(w, r, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	var authErr *sigv4.Error
	if errors.As(err, &authErr) {
		message := "The request could not be authenticated."
		if authErr.Code == "RequestTimeTooSkewed" {
			message = "The request timestamp is outside the accepted clock tolerance."
		}
		writeS3Error(w, r, authErr.Status, authErr.Code, message)
		return
	}
	status, code, message := http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again."
	switch {
	case errors.Is(err, storage.ErrBucketNotFound):
		status, code, message = http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist."
	case errors.Is(err, storage.ErrObjectNotFound), errors.Is(err, storage.ErrContentNotFound):
		status, code, message = http.StatusNotFound, "NoSuchKey", "The specified key does not exist."
	case errors.Is(err, storage.ErrBucketNotEmpty):
		status, code, message = http.StatusConflict, "BucketNotEmpty", "The bucket you tried to delete is not empty."
	case errors.Is(err, storage.ErrAlreadyExists):
		status, code, message = http.StatusConflict, "BucketAlreadyExists", "The requested bucket name is not available."
	case errors.Is(err, storage.ErrInvalidBucketName):
		status, code, message = http.StatusBadRequest, "InvalidBucketName", "The specified bucket is not valid."
	case errors.Is(err, storage.ErrInvalidPageLimit), errors.Is(err, storage.ErrInvalidMetadata):
		status, code, message = http.StatusBadRequest, "InvalidArgument", "The request contains an invalid argument."
	case errors.Is(err, storage.ErrConflict):
		status, code, message = http.StatusConflict, "OperationAborted", "The operation conflicts with the current resource state."
	}
	writeS3Error(w, r, status, code, message)
}

func writeS3Error(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	requestID := requestID(w)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	body, err := xml.Marshal(errorResponse{Code: code, Message: message, Resource: r.URL.EscapedPath(), RequestID: requestID})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>%s", body)
}

func requestID(w http.ResponseWriter) string {
	if existing := w.Header().Get("x-amz-request-id"); existing != "" {
		return existing
	}
	var identifier [12]byte
	if _, err := rand.Read(identifier[:]); err != nil {
		copy(identifier[:], []byte("stillfs-error"))
	}
	identifierText := hex.EncodeToString(identifier[:])
	w.Header().Set("x-amz-request-id", identifierText)
	return identifierText
}

func writeXML(w http.ResponseWriter, r *http.Request, status int, value interface{}) {
	body, err := xml.Marshal(value)
	if err != nil {
		writeS3Error(w, r, http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again.")
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>%s", body)
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	writeS3Error(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed for this resource.")
}
