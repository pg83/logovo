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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

var errNotFound = errors.New("not found")

const s3CallTimeout = 10 * time.Minute

type objectStore interface {
	put(key string, data []byte)
	get(key string) []byte

	open(key string) io.ReadCloser
	putFile(key, path string)
	list(prefix string) []object
	del(key string)

	stat(key string) (string, bool)
}

type object struct {
	key  string
	size int64
}

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
	durableDirectory(root)

	return &dirStore{root: root}
}

func (d *dirStore) path(key string) string {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		throwFmt("store: bad key %q", key)
	}

	return filepath.Join(d.root, filepath.FromSlash(key))
}

func (d *dirStore) put(key string, data []byte) {
	d.writeObject(key, bytes.NewReader(data))
}

func (d *dirStore) writeObject(key string, src io.Reader) {
	p := d.path(key)
	dir := filepath.Dir(p)

	durableDirectory(dir)

	f := throw2(os.CreateTemp(dir, "."+filepath.Base(p)+"-*.tmp"))

	defer f.Close()

	defer os.Remove(f.Name())

	throw(f.Chmod(0644))
	throw2(io.Copy(f, src))
	throw(f.Sync())
	throw(f.Close())
	throw(os.Rename(f.Name(), p))
	syncParents(dir)
}

func syncParents(path string) {
	path = throw2(filepath.Abs(path))

	for {
		syncDirectory(path)
		parent := filepath.Dir(path)

		if parent == path {
			return
		}

		path = parent
	}
}

func syncDirectory(path string) {
	f := throw2(os.Open(path))

	defer f.Close()

	throw(f.Sync())
}

func durableDirectory(path string) {
	info, err := os.Stat(path)

	if err == nil {
		if !info.IsDir() {
			throwFmt("store: not a directory: %s", path)
		}

		return
	}

	if !errors.Is(err, fs.ErrNotExist) {
		throw(err)
	}

	parent := filepath.Dir(path)

	durableDirectory(parent)
	err = os.Mkdir(path, 0755)

	if err != nil && !errors.Is(err, fs.ErrExist) {
		throw(err)
	}

	syncDirectory(parent)
}

func (d *dirStore) get(key string) []byte {
	data, err := os.ReadFile(d.path(key))

	if errors.Is(err, fs.ErrNotExist) {
		throw(errNotFound)
	}

	throw(err)

	return data
}

func (d *dirStore) open(key string) io.ReadCloser {
	f, err := os.Open(d.path(key))

	if errors.Is(err, fs.ErrNotExist) {
		throw(errNotFound)
	}

	throw(err)

	return f
}

func (d *dirStore) putFile(key, path string) {
	f := throw2(os.Open(path))

	defer f.Close()

	d.writeObject(key, f)
}

func (d *dirStore) list(prefix string) []object {
	var objects []object

	err := filepath.WalkDir(d.root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || strings.HasSuffix(p, ".tmp") {
			return nil
		}

		key := filepath.ToSlash(throw2(filepath.Rel(d.root, p)))

		if strings.HasPrefix(key, prefix) {
			info := throw2(entry.Info())

			objects = append(objects, object{key: key, size: info.Size()})
		}

		return nil
	})

	throw(err)
	sort.Slice(objects, func(i, j int) bool { return objects[i].key < objects[j].key })

	return objects
}

func (d *dirStore) del(key string) {
	err := os.Remove(d.path(key))

	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		throw(err)
	}

	if err == nil {
		syncDirectory(filepath.Dir(d.path(key)))
	}
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

func s3ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s3CallTimeout)
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
	ctx, cancel := s3ctx()

	defer cancel()

	throw2(s.cli.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	}))
}

func (s *s3Store) get(key string) []byte {
	ctx, cancel := s3ctx()

	defer cancel()

	resp, err := s.cli.GetObject(ctx, &s3.GetObjectInput{
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

type s3Body struct {
	io.ReadCloser
	cancel context.CancelCauseFunc
}

func s3Timeout(cancel context.CancelCauseFunc) *time.Timer {
	return time.AfterFunc(s3CallTimeout, func() { cancel(context.DeadlineExceeded) })
}

func (b *s3Body) read(p []byte) (int, error) {
	t := s3Timeout(b.cancel)

	defer t.Stop()

	return b.ReadCloser.Read(p)
}

func (b *s3Body) Read(p []byte) (int, error) {
	return b.read(p)
}

func (b *s3Body) close() error {
	defer b.cancel(nil)

	return b.ReadCloser.Close()
}

func (b *s3Body) Close() error {
	return b.close()
}

func (s *s3Store) open(key string) io.ReadCloser {
	ctx, cancel := context.WithCancelCause(context.Background())
	t := s3Timeout(cancel)

	resp, err := s.cli.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})

	t.Stop()

	if err != nil {
		cancel(nil)
	}

	if isS3NotFound(err) {
		throw(errNotFound)
	}

	throw(err)

	return &s3Body{ReadCloser: resp.Body, cancel: cancel}
}

func (s *s3Store) putFile(key, path string) {
	f := throw2(os.Open(path))

	defer f.Close()

	ctx, cancel := s3ctx()

	defer cancel()

	throw2(s.cli.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   f,
	}))
}

func (s *s3Store) list(prefix string) []object {
	ctx, cancel := s3ctx()

	defer cancel()

	var objects []object
	var token *string

	for {
		page := throw2(s.cli.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		}))

		for _, obj := range page.Contents {
			objects = append(objects, object{key: aws.ToString(obj.Key), size: aws.ToInt64(obj.Size)})
		}

		if !aws.ToBool(page.IsTruncated) {
			break
		}

		token = page.NextContinuationToken
	}

	sort.Slice(objects, func(i, j int) bool { return objects[i].key < objects[j].key })

	return objects
}

func (s *s3Store) del(key string) {
	ctx, cancel := s3ctx()

	defer cancel()

	throw2(s.cli.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}))
}

func (s *s3Store) stat(key string) (string, bool) {
	ctx, cancel := s3ctx()

	defer cancel()

	resp, err := s.cli.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})

	if isS3NotFound(err) {
		return "", false
	}

	throw(err)

	return aws.ToString(resp.ETag) + "/" + itoa(aws.ToInt64(resp.ContentLength)), true
}
