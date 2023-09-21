package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Storage is a storage backed by AWS S3
type S3Storage struct {
	Storage
	bucket      string
	s3          *s3.Client
	logger      *log.Logger
	purgeDays   time.Duration
	noMultipart bool
}

// NewS3Storage is the factory for S3Storage
func NewS3Storage(ctx context.Context, accessKey, secretKey, bucketName string, purgeDays int, region, endpoint string, disableMultipart bool, forcePathStyle bool, logger *log.Logger) (*S3Storage, error) {
	cfg, err := getAwsConfig(ctx, accessKey, secretKey)
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.Region = region
		o.UsePathStyle = forcePathStyle
		if len(endpoint) > 0 {
			o.EndpointResolver = s3.EndpointResolverFromURL(endpoint)
		}
	})

	return &S3Storage{
		bucket:      bucketName,
		s3:          client,
		logger:      logger,
		noMultipart: disableMultipart,
		purgeDays:   time.Duration(purgeDays*24) * time.Hour,
	}, nil
}

// Type returns the storage type
func (s *S3Storage) Type() string {
	return "s3"
}

// Head retrieves content length of a file from storage
func (s *S3Storage) Head(ctx context.Context, token string, filename string) (contentLength uint64, err error) {

	_, selectedKey, err := s.getFileName(ctx, filename, s.bucket, token)
	if err != nil {
		return
	}

	if selectedKey == "" {
		// file not found
		return
	}

	getRequest := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(selectedKey),
	}

	response, err := s.s3.GetObject(ctx, getRequest)
	if err != nil {
		return
	}

	contentLength = uint64(response.ContentLength)
	return
}

// Purge cleans up the storage
func (s *S3Storage) Purge(context.Context, time.Duration) (err error) {
	// NOOP expiration is set at upload time
	return nil
}

// IsNotExist indicates if a file doesn't exist on storage
func (s *S3Storage) IsNotExist(err error) bool {
	if err == nil {
		return false
	}

	var nkerr *types.NoSuchKey
	return errors.As(err, &nkerr)
}

// Get retrieves a file from storage
func (s *S3Storage) Get(ctx context.Context, token string, filename string, rng *Range) (reader io.ReadCloser, fName string, contentLength uint64, err error) {

	fName, selectedKey, err := s.getFileName(ctx, filename, s.bucket, token)
	if err != nil {
		return
	}

	if selectedKey == "" {
		// file not found
		return
	}

	getRequest := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(selectedKey),
	}

	response, err := s.s3.GetObject(ctx, getRequest)
	if err != nil {
		return
	}

	contentLength = uint64(response.ContentLength)
	if rng != nil && response.ContentRange != nil {
		rng.SetContentRange(*response.ContentRange)
	}

	reader = response.Body
	return
}

func (s *S3Storage) getFileName(ctx context.Context, metadataFlag, bucket, token string) (fileName, selectedKey string, err error) {

	key := fmt.Sprintf("%s/", token)

	listInput := &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(key),
	}

	result, err := s.s3.ListObjectsV2(ctx, listInput)
	if err != nil {
		return
	}

	for _, item := range result.Contents {
		itemkey := *item.Key
		if metadataFlag == ".metadata" && strings.HasSuffix(itemkey, ".metadata") {
			selectedKey = itemkey
			break
		} else if metadataFlag != ".metadata" && !strings.HasSuffix(itemkey, ".metadata") {
			selectedKey = itemkey
			parts := strings.Split(itemkey, "/")
			name := parts[len(parts)-1]
			fileName = name
			break
		}
	}

	return
}

// Delete removes a file from storage
func (s *S3Storage) Delete(ctx context.Context, token string) (err error) {

	_, selectedKey, err := s.getFileName(ctx, ".metadata", s.bucket, token)
	if err != nil {
		return
	}
	deleteRequest := &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(selectedKey),
	}

	_, err = s.s3.DeleteObject(ctx, deleteRequest)
	if err != nil {
		return
	}

	_, selectedKey, err = s.getFileName(ctx, "", s.bucket, token)
	if err != nil {
		return
	}

	//key := fmt.Sprintf("%s/%s", token, filename)
	deleteRequest = &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(selectedKey),
	}

	_, err = s.s3.DeleteObject(ctx, deleteRequest)

	return
}

// Put saves a file on storage
func (s *S3Storage) Put(ctx context.Context, token string, filename string, reader io.Reader, contentType string, _ uint64) (err error) {
	key := fmt.Sprintf("%s/%s", token, filename)

	s.logger.Printf("Uploading file %s to S3 Bucket", filename)
	var concurrency int
	if !s.noMultipart {
		concurrency = 20
	} else {
		concurrency = 1
	}

	// Create an uploader with the session and custom options
	uploader := manager.NewUploader(s.s3, func(u *manager.Uploader) {
		u.Concurrency = concurrency // default is 5
		u.LeavePartsOnError = false
	})

	var expire *time.Time
	if s.purgeDays.Hours() > 0 {
		expire = aws.Time(time.Now().Add(s.purgeDays))
	}

	_, err = uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        reader,
		Expires:     expire,
		ContentType: aws.String(contentType),
	})

	return
}

func (s *S3Storage) IsRangeSupported() bool { return true }

func getAwsConfig(ctx context.Context, accessKey, secretKey string) (aws.Config, error) {
	return config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(credentials.StaticCredentialsProvider{
			Value: aws.Credentials{
				AccessKeyID:     accessKey,
				SecretAccessKey: secretKey,
				SessionToken:    "",
			},
		}),
	)
}
