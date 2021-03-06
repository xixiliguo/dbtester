// Copyright 2016 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package remotestorage

import (
	"context"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"

	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jwt"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

// Uploader defines storage uploader.
type Uploader interface {
	// UploadFile uploads a file.
	UploadFile(bucket, src, dst string, opts ...OpOption) error

	// UploadDir uploads a directory.
	UploadDir(bucket, src, dst string, opts ...OpOption) error
}

// GoogleCloudStorage wraps Google Cloud Storage API.
type GoogleCloudStorage struct {
	JSONKey []byte
	Project string
	Config  *jwt.Config
}

// NewGoogleCloudStorage creates a new uploader.
func NewGoogleCloudStorage(key []byte, project string) (Uploader, error) {
	conf, err := google.JWTConfigFromJSON(
		key,
		storage.ScopeFullControl,
	)
	if err != nil {
		return nil, err
	}
	return &GoogleCloudStorage{
		JSONKey: key,
		Project: project,
		Config:  conf,
	}, nil
}

// UploadFile uploads a file to Google Cloud Storage.
func (g *GoogleCloudStorage) UploadFile(bucket, src, dst string, opts ...OpOption) error {
	if g == nil {
		return fmt.Errorf("GoogleCloudStorage is nil")
	}
	ret := &Op{}
	ret.applyOpts(opts)

	ctx := context.Background()

	client, err := storage.NewClient(ctx, option.WithTokenSource(g.Config.TokenSource(ctx)))
	if err != nil {
		return err
	}
	defer client.Close()

	bkt := client.Bucket(bucket)
	if err := bkt.Create(ctx, g.Project, nil); err != nil {
		if !strings.Contains(err.Error(), "You already own this bucket. Please select another name") {
			return err
		}
	}

	wc := client.Bucket(bucket).Object(dst).NewWriter(context.Background())
	if ret.ContentType != "" {
		wc.ContentType = ret.ContentType
	}

	plog.Printf("uploading %q ---> %q", src, dst)
	bts, err := ioutil.ReadFile(src)
	if err != nil {
		return fmt.Errorf("ioutil.ReadFile(%s) %v", src, err)
	}
	if _, err := wc.Write(bts); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	plog.Printf("finished uploading %q", src)

	return nil
}

// UploadDir uploads a directory to Google Cloud Storage.
func (g *GoogleCloudStorage) UploadDir(bucket, src, dst string, opts ...OpOption) error {
	if g == nil {
		return fmt.Errorf("GoogleCloudStorage is nil")
	}
	ret := &Op{}
	ret.applyOpts(opts)

	ctx := context.Background()

	client, err := storage.NewClient(ctx, option.WithTokenSource(g.Config.TokenSource(ctx)))
	if err != nil {
		return err
	}
	defer client.Close()

	bkt := client.Bucket(bucket)
	if err := bkt.Create(ctx, g.Project, nil); err != nil {
		if !strings.Contains(err.Error(), "You already own this bucket. Please select another name") {
			return err
		}
	}

	fmap, err := walkRecursive(src)
	if err != nil {
		return err
	}

	donec, errc := make(chan struct{}), make(chan error)
	for fpath := range fmap {
		go func(fpath string) {
			targetPath := filepath.Join(dst, strings.Replace(fpath, src, "", -1))

			plog.Printf("uploading %q ---> %q", fpath, targetPath)
			wc := client.Bucket(bucket).Object(targetPath).NewWriter(context.Background())
			if ret.ContentType != "" {
				wc.ContentType = ret.ContentType
			}
			bts, err := ioutil.ReadFile(fpath)
			if err != nil {
				errc <- fmt.Errorf("ioutil.ReadFile(%s) %v", fpath, err)
				return
			}
			if _, err := wc.Write(bts); err != nil {
				errc <- err
				return
			}
			if err := wc.Close(); err != nil {
				errc <- err
				return
			}
			plog.Printf("uploaded %q ---> %q", fpath, targetPath)

			donec <- struct{}{}
		}(fpath)
	}

	cnt, num := 0, len(fmap)
	for cnt != num {
		select {
		case <-donec:
		case err := <-errc:
			return err
		}
		cnt++
	}

	plog.Printf("finished uploading %q", src)
	return nil
}
