package gateway

import (
	"encoding/xml"
	"time"
)

type ListObjectsV2Output struct {
	XMLName               xml.Name               `xml:"ListBucketResult"`
	Name                  string                 `xml:"Name"`
	Prefix                string                 `xml:"Prefix"`
	StartAfter            string                 `xml:"StartAfter,omitempty"`
	ContinuationToken     string                 `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string                 `xml:"NextContinuationToken,omitempty"`
	KeyCount              int                    `xml:"KeyCount"`
	MaxKeys               int                    `xml:"MaxKeys"`
	Delimiter             string                 `xml:"Delimiter,omitempty"`
	IsTruncated           bool                   `xml:"IsTruncated"`
	Contents              []ListObjectsV2Content `xml:"Contents,omitempty"`
	CommonPrefixes        []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes,omitempty"`
}

type ListObjectsV2Content struct {
	Key          string    `xml:"Key"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag"`
	Size         int64     `xml:"Size"`
	StorageClass string    `xml:"StorageClass"`
	Owner        *struct {
		ID          string `xml:"ID"`
		DisplayName string `xml:"DisplayName"`
	} `xml:"Owner,omitempty"`
}

type ListObjectsOutput struct {
	XMLName        xml.Name               `xml:"ListBucketResult"`
	Name           string                 `xml:"Name"`
	Prefix         string                 `xml:"Prefix"`
	Marker         string                 `xml:"Marker"`
	MaxKeys        int                    `xml:"MaxKeys"`
	Delimiter      string                 `xml:"Delimiter,omitempty"`
	IsTruncated    bool                   `xml:"IsTruncated"`
	NextMarker     string                 `xml:"NextMarker,omitempty"`
	Contents       []ListObjectsV2Content `xml:"Contents,omitempty"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes,omitempty"`
}

type ListBucketsOutput struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Owner   struct {
		ID          string `xml:"ID"`
		DisplayName string `xml:"DisplayName"`
	} `xml:"Owner"`
	Buckets []struct {
		Bucket struct {
			Name         string    `xml:"Name"`
			CreationDate time.Time `xml:"CreationDate"`
		} `xml:"Bucket"`
	} `xml:"Buckets>Bucket"`
}

type ErrorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	RequestID string   `xml:"RequestId"`
}

type DeleteObjectsRequest struct {
	XMLName xml.Name              `xml:"Delete"`
	Objects []DeleteObjectsObject `xml:"Object"`
	Quiet   bool                  `xml:"Quiet,omitempty"`
}

type DeleteObjectsObject struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
}

type DeleteObjectsResult struct {
	XMLName xml.Name               `xml:"DeleteResult"`
	Deleted []DeleteObjectsDeleted `xml:"Deleted"`
	Error   []DeleteObjectsError   `xml:"Error,omitempty"`
}

type DeleteObjectsDeleted struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
}

type DeleteObjectsError struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

type CopyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}
