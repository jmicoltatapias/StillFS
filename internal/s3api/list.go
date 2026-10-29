package s3api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jmicoltatapias/stillfs/internal/storage"
)

type listBucketsResult struct {
	XMLName xml.Name      `xml:"ListAllMyBucketsResult"`
	XMLNS   string        `xml:"xmlns,attr"`
	Owner   ownerResult   `xml:"Owner"`
	Buckets bucketsResult `xml:"Buckets"`
}

type ownerResult struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName,omitempty"`
}

type bucketsResult struct {
	Items []listedBucket `xml:"Bucket"`
}

type listedBucket struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type listBucketResult struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	XMLNS                 string         `xml:"xmlns,attr"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	MaxKeys               int            `xml:"MaxKeys"`
	KeyCount              int            `xml:"KeyCount"`
	IsTruncated           bool           `xml:"IsTruncated"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
	Contents              []listedObject `xml:"Contents,omitempty"`
	CommonPrefixes        []commonPrefix `xml:"CommonPrefixes,omitempty"`
}

type listedObject struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag,omitempty"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

type logicalListItem struct {
	object *storage.ObjectMetadata
	prefix string
	cursor string
}

type continuation struct {
	Version   int    `json:"v"`
	After     string `json:"a"`
	Prefix    string `json:"p"`
	Delimiter string `json:"d"`
}

func (s *Server) listObjects(ctx context.Context, principalID int64, bucket string, query url.Values) (listBucketResult, error) {
	allowed := map[string]bool{"list-type": true, "prefix": true, "delimiter": true, "max-keys": true, "continuation-token": true, "start-after": true, "x-id": true}
	for key, values := range query {
		if !allowed[key] || len(values) != 1 {
			return listBucketResult{}, &requestAPIError{Status: http.StatusBadRequest, Code: "InvalidArgument", Message: "Unsupported or repeated ListObjectsV2 parameter."}
		}
	}
	if operation, exists := query["x-id"]; exists && (len(operation) != 1 || operation[0] != "ListObjectsV2") {
		return listBucketResult{}, &requestAPIError{Status: http.StatusBadRequest, Code: "InvalidArgument", Message: "x-id does not match ListObjectsV2."}
	}
	prefix := query.Get("prefix")
	delimiter := query.Get("delimiter")
	if len(prefix)+len(delimiter) > s.maxMetadata || !utf8Text(prefix) || !utf8Text(delimiter) {
		return listBucketResult{}, &requestAPIError{Status: http.StatusBadRequest, Code: "MetadataTooLarge", Message: "Listing metadata exceeds the configured limit."}
	}
	maxKeys := 1000
	if raw := query.Get("max-keys"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000 {
			return listBucketResult{}, &requestAPIError{Status: http.StatusBadRequest, Code: "InvalidArgument", Message: "max-keys must be between 1 and 1000."}
		}
		maxKeys = parsed
	}
	after := query.Get("start-after")
	if token := query.Get("continuation-token"); token != "" {
		if after != "" {
			return listBucketResult{}, &requestAPIError{Status: http.StatusBadRequest, Code: "InvalidArgument", Message: "start-after and continuation-token cannot be combined."}
		}
		decoded, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			return listBucketResult{}, &requestAPIError{Status: http.StatusBadRequest, Code: "InvalidToken", Message: "The continuation token is invalid."}
		}
		var cursor continuation
		if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.Version != 1 || cursor.Prefix != prefix || cursor.Delimiter != delimiter || cursor.After == "" {
			return listBucketResult{}, &requestAPIError{Status: http.StatusBadRequest, Code: "InvalidToken", Message: "The continuation token is invalid for this listing."}
		}
		after = cursor.After
	}
	if !utf8Text(after) || len(after) > 1024 {
		return listBucketResult{}, &requestAPIError{Status: http.StatusBadRequest, Code: "InvalidArgument", Message: "start-after is invalid."}
	}
	if _, err := s.ownedBucket(ctx, principalID, bucket); err != nil {
		return listBucketResult{}, err
	}

	items := make([]logicalListItem, 0, maxKeys+1)
	var pendingPrefix, pendingCursor string
	truncated := false
	scanAfter := after
	for {
		page, err := s.metadata.List(ctx, bucket, storage.ObjectPageRequest{Prefix: prefix, After: scanAfter, Limit: 1000})
		if err != nil {
			return listBucketResult{}, err
		}
		if len(page.Items) == 0 {
			break
		}
		stop := false
		for i := range page.Items {
			object := page.Items[i]
			common := ""
			if delimiter != "" {
				remainder := strings.TrimPrefix(object.Key, prefix)
				if index := strings.Index(remainder, delimiter); index >= 0 {
					common = prefix + remainder[:index+len(delimiter)]
				}
			}
			if pendingPrefix != "" && common != pendingPrefix {
				items = append(items, logicalListItem{prefix: pendingPrefix, cursor: pendingCursor})
				pendingPrefix, pendingCursor = "", ""
				if len(items) > maxKeys {
					stop = true
					break
				}
			}
			if common != "" {
				pendingPrefix, pendingCursor = common, object.Key
			} else {
				copy := object
				items = append(items, logicalListItem{object: &copy, cursor: object.Key})
				if len(items) > maxKeys {
					stop = true
					break
				}
			}
			scanAfter = object.Key
		}
		if stop {
			truncated = true
			break
		}
		if page.NextAfter == "" {
			break
		}
		scanAfter = page.NextAfter
	}
	if !truncated && pendingPrefix != "" {
		items = append(items, logicalListItem{prefix: pendingPrefix, cursor: pendingCursor})
	}
	if len(items) > maxKeys {
		items = items[:maxKeys]
		truncated = true
	}
	result := listBucketResult{
		XMLNS: xmlNamespace, Name: bucket, Prefix: prefix, Delimiter: delimiter,
		MaxKeys: maxKeys, KeyCount: len(items), IsTruncated: truncated,
		ContinuationToken: query.Get("continuation-token"), StartAfter: query.Get("start-after"),
		Contents: make([]listedObject, 0), CommonPrefixes: make([]commonPrefix, 0),
	}
	for _, item := range items {
		if item.object == nil {
			result.CommonPrefixes = append(result.CommonPrefixes, commonPrefix{Prefix: item.prefix})
			continue
		}
		result.Contents = append(result.Contents, listedObject{
			Key: item.object.Key, LastModified: item.object.UpdatedAt.UTC().Format(time.RFC3339),
			ETag: item.object.ETag, Size: item.object.ContentLength, StorageClass: "STANDARD",
		})
	}
	if truncated && len(items) > 0 {
		cursor := continuation{Version: 1, After: items[len(items)-1].cursor, Prefix: prefix, Delimiter: delimiter}
		encoded, err := json.Marshal(cursor)
		if err != nil {
			return listBucketResult{}, fmt.Errorf("encode continuation cursor: %w", err)
		}
		result.NextContinuationToken = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return result, nil
}

func utf8Text(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if (character >= 0 && character <= 8) || character == 11 || character == 12 || (character >= 14 && character <= 31) || character == 0xfffe || character == 0xffff {
			return false
		}
	}
	return true
}
