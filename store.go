package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// objectStore is the little bit of S3 the pipeline needs. dir: is the
// same layout on a local directory, for tests and for poking at things.
type objectStore interface {
	put(key string, data []byte)
	get(key string) []byte
	list(prefix string) []string
	del(key string)
	appendTo(key string, data []byte)
	// stat returns a value that changes whenever the object changes and
	// whether the object exists at all.
	stat(key string) (string, bool)
}

var errNotFound = errors.New("not found")

func openStore(spec string) objectStore {
	switch {
	case strings.HasPrefix(spec, "dir:"):
		return newDirStore(spec[len("dir:"):])
	case strings.HasPrefix(spec, "s3://"):
		return newS3Store(spec[len("s3://"):])
	}

	throwFmt("store: want dir:/path or s3://bucket, got %q", spec)

	return nil
}

type dirStore struct {
	root string
}

func newDirStore(root string) *dirStore {
	throw(os.MkdirAll(root, 0755))

	return &dirStore{root: root}
}

func (d *dirStore) path(key string) string {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		throwFmt("store: bad key %q", key)
	}

	return filepath.Join(d.root, filepath.FromSlash(key))
}

func (d *dirStore) put(key string, data []byte) {
	p := d.path(key)
	throw(os.MkdirAll(filepath.Dir(p), 0755))
	tmp := p + ".tmp"
	throw(os.WriteFile(tmp, data, 0644))
	throw(os.Rename(tmp, p))
}

func (d *dirStore) get(key string) []byte {
	data, err := os.ReadFile(d.path(key))

	if errors.Is(err, fs.ErrNotExist) {
		throw(errNotFound)
	}

	throw(err)

	return data
}

func (d *dirStore) list(prefix string) []string {
	var keys []string

	err := filepath.WalkDir(d.root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || strings.HasSuffix(p, ".tmp") {
			return nil
		}

		key := filepath.ToSlash(throw2(filepath.Rel(d.root, p)))

		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}

		return nil
	})

	throw(err)
	sort.Strings(keys)

	return keys
}

func (d *dirStore) del(key string) {
	err := os.Remove(d.path(key))

	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		throw(err)
	}
}

func (d *dirStore) appendTo(key string, data []byte) {
	p := d.path(key)
	throw(os.MkdirAll(filepath.Dir(p), 0755))
	f := throw2(os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644))
	_, werr := f.Write(data)
	cerr := f.Close()
	throw(werr)
	throw(cerr)
}

func (d *dirStore) stat(key string) (string, bool) {
	info, err := os.Stat(d.path(key))

	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}

	throw(err)

	return info.ModTime().UTC().Format("20060102T150405.000000000") + "/" + itoa(info.Size()), true
}

type s3Store struct {
	bucket string
	cli    *s3.Client
}

func newS3Store(bucket string) *s3Store {
	key := os.Getenv("AWS_ACCESS_KEY_ID")
	secret := os.Getenv("AWS_SECRET_ACCESS_KEY")
	region := os.Getenv("AWS_REGION")
	endpoint := os.Getenv("S3_ENDPOINT")

	if key == "" || secret == "" {
		throwFmt("store: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are required for s3://")
	}

	if region == "" {
		region = "us-east-1"
	}

	cfg := aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(key, secret, ""),
	}

	cli := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}

		o.UsePathStyle = true
	})

	return &s3Store{bucket: bucket, cli: cli}
}

func isS3NotFound(err error) bool {
	var nf *types.NotFound

	if errors.As(err, &nf) {
		return true
	}

	var nsk *types.NoSuchKey

	if errors.As(err, &nsk) {
		return true
	}

	var ae smithy.APIError

	if errors.As(err, &ae) {
		code := ae.ErrorCode()

		return code == "NotFound" || code == "NoSuchKey"
	}

	return false
}

func (s *s3Store) put(key string, data []byte) {
	throw2(s.cli.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	}))
}

func (s *s3Store) get(key string) []byte {
	resp, err := s.cli.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})

	if isS3NotFound(err) {
		throw(errNotFound)
	}

	throw(err)
	defer resp.Body.Close()

	return throw2(io.ReadAll(resp.Body))
}

func (s *s3Store) list(prefix string) []string {
	var keys []string
	var token *string

	for {
		page := throw2(s.cli.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		}))

		for _, obj := range page.Contents {
			keys = append(keys, aws.ToString(obj.Key))
		}

		if !aws.ToBool(page.IsTruncated) {
			break
		}

		token = page.NextContinuationToken
	}

	sort.Strings(keys)

	return keys
}

func (s *s3Store) del(key string) {
	throw2(s.cli.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}))
}

// appendTo is read, concatenate, write: S3 has no append. Sessions are
// small enough for that today; a multipart copy is the upgrade path.
func (s *s3Store) appendTo(key string, data []byte) {
	var old []byte

	exc := try(func() {
		old = s.get(key)
	})

	if exc != nil && !errors.Is(exc.asError(), errNotFound) {
		exc.throw()
	}

	s.put(key, append(old, data...))
}

func (s *s3Store) stat(key string) (string, bool) {
	resp, err := s.cli.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})

	if isS3NotFound(err) {
		return "", false
	}

	throw(err)

	return aws.ToString(resp.ETag) + "/" + itoa(aws.ToInt64(resp.ContentLength)), true
}
