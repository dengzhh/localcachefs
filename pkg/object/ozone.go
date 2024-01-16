//go:build !nohdfs
// +build !nohdfs

/*
 * JuiceFS, Copyright 2020 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package object

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/apache/ozone-go/api"
	"github.com/apache/ozone-go/api/datanode"
	ozone_proto "github.com/apache/ozone-go/api/proto/ozone"
	"github.com/colinmarc/hdfs/v2"
	"github.com/colinmarc/hdfs/v2/hadoopconf"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var chunkPool = sync.Pool{
	New: func() interface{} {
		//buf := make([]byte, 32<<10)
		buf := make([]byte, 4<<20)
		return &buf
	},
}

type OfsClient struct {
	DefaultObjectStorage
	addr string
	c    *api.OzoneClient
}

type O3fsClient struct {
	DefaultObjectStorage
	addr     string
	volume   string
	bucket   string
	ozClient *api.OzoneClient
}

type O3file struct {
	o3fsClient  *O3fsClient
	name        string
	position    int64
	size        int64
	chunkBuffer *bytes.Buffer
	dnClient    *datanode.DatanodeClient

	// chunkOffsets[i] stores the index of the first data byte in
	// chunkStream i w.r.t the block data.
	// Let’s say we have chunk size as 40 bytes. And let's say the parent
	// block stores data from index 200 and has length 400.
	// The first 40 bytes of this block will be stored in chunk[0], next 40 in
	// chunk[1] and so on. But since the chunkOffsets are w.r.t the block only
	// and not the key, the values in chunkOffsets will be [0, 40, 80,....].
	//chunkOffsets []int64

	// Index of the chunkStream corresponding to the current position of the
	// BlockInputStream i.e offset of the data to be read next from this block
	chunkIndex int
	blockIndex int

	keyInfo *ozone_proto.KeyInfo
	chunks  []datanode.ChunkInfo
	//buffer       []bytes.Buffer
	//buffer2      []byte
	//chunkBuffers [][0x400000]byte
}

func OpenO3file(o *O3fsClient, name string) (*O3file, error) {
	keyInfo, err := o.ozClient.OmClient.GetKey(o.volume, o.bucket, name)
	if err != nil {
		return nil, err
	}
	o3file := &O3file{
		o3fsClient:  o,
		position:    0,
		size:        int64(*keyInfo.DataSize),
		chunkIndex:  0,
		blockIndex:  0,
		chunks:      nil,
		chunkBuffer: nil,
		name:        name,
		keyInfo:     keyInfo,
		dnClient:    nil}
	return o3file, nil
}

func (f *O3file) nextChunk() (err error) {
	if f.chunkIndex == len(f.chunks)-1 {
		if f.blockIndex == len(f.keyInfo.KeyLocationList[0].KeyLocations)-1 {
			f.dnClient.Close()
			f.dnClient = nil
			return io.EOF
		}
		if f.dnClient != nil {
			f.dnClient.Close()
			f.dnClient = nil
		}
		f.blockIndex += 1
		f.chunks = nil
		f.chunkIndex = 0
	} else {
		f.chunkIndex += 1
	}
	//f.chunkBuffer.Reset()
	f.chunkBuffer = nil
	return nil
}

func (f *O3file) Seek(offset int64, whence int) (int64, error) {
	var off int64
	switch whence {
	case io.SeekStart:
		off = offset
	case io.SeekCurrent:
		off = f.position + offset
	case io.SeekEnd:
		off = f.size + offset
	default:
		return f.position, fmt.Errorf("invalid whence: %d", whence)
	}

	if off < 0 || off > int64(f.size) {
		return f.position, fmt.Errorf("invalid resulting offset: %d", off)
	}

	if int64(f.position) != off {
		f.position = off

		// TODO: to skip block & chunk, update blockIndex/chunkIndex/chunkBuffer
		var blockIndex int = 0
		var chunkIndex int = 0
		var blockPosition int64 = 0
		var chunkPosition int64 = 0
		for _, location := range f.keyInfo.KeyLocationList[0].KeyLocations {
			if f.position >= (blockPosition + int64(*location.Length)) {
				blockIndex += 1
				blockPosition += int64(*location.Length)
				continue
			}
			pipeline := location.Pipeline
			dnBlockId := api.ConvertBlockId(location.BlockID)
			dnClient, err := datanode.CreateDatanodeClient(pipeline, datanode.STANDALONE_RANDOM)
			chunks, err := dnClient.GetBlock(dnBlockId)
			if err != nil {
				return int64(f.position), err
			}
			chunkIndex = 0
			chunkPosition = 0
			for _, chunk := range chunks {
				if f.position < (blockPosition + chunkPosition + int64(chunk.Len)) {
					data, err := dnClient.ReadChunk(dnBlockId, chunk)
					if err != nil {
						return f.position, err
					}
					f.chunkBuffer = bytes.NewBuffer(data[f.position-blockPosition-chunkPosition:])
					f.chunks = chunks
					f.dnClient = dnClient
					f.blockIndex = blockIndex
					f.chunkIndex = chunkIndex
					return f.position, nil
				}
				chunkIndex += 1
				chunkPosition += int64(chunk.Len)
			}
			break
		}
	}

	return f.position, fmt.Errorf("Seems file size differ ozone size")
}

func (f *O3file) Read(p []byte) (n int, err error) {
	var count int = 0
	var pos int = 0
	if len(f.keyInfo.KeyLocationList) == 0 {
		return 0, errors.New("Get key returned with zero key location version " + f.o3fsClient.volume + "/" + f.o3fsClient.bucket + "/" + f.name)
	}

	if len(f.keyInfo.KeyLocationList[0].KeyLocations) == 0 {
		return 0, errors.New("Key location doesn't have any datanode for key " + f.o3fsClient.volume + "/" + f.o3fsClient.bucket + "/" + f.name)
	}

	if f.size <= f.position {
		if len(p) == 0 {
			return 0, nil
		}
		return 0, io.EOF
	}

	for f.size > f.position {
		if f.chunkBuffer == nil {
			location := f.keyInfo.KeyLocationList[0].KeyLocations[f.blockIndex]
			pipeline := location.Pipeline
			dnBlockId := api.ConvertBlockId(location.BlockID)
			if f.dnClient == nil {
				dnClient, err := datanode.CreateDatanodeClient(pipeline, datanode.STANDALONE_RANDOM)
				if err != nil {
					return count, err
				}
				f.dnClient = dnClient
			}
			if f.chunks == nil {
				chunks, err := f.dnClient.GetBlock(dnBlockId)
				if err != nil {
					return count, err
				}
				f.chunks = chunks
			}
			chunk := f.chunks[f.chunkIndex]
			data, err := f.dnClient.ReadChunk(dnBlockId, chunk)
			if err != nil {
				return count, err
			}
			f.chunkBuffer = bytes.NewBuffer(data)
		}
		for f.chunkBuffer.Len() > 0 {
			n, err := f.chunkBuffer.Read(p[pos:])
			f.position += int64(n)
			count += n
			if err != nil || n < len(p[pos:]) { // EOF
				err := f.nextChunk()
				if err != nil {
					return count, err
				}
			}
			pos += n
		}
	}
	return count, nil
}

func (f *O3file) Close() error {
	// TODO: commitKey
	return nil
}

func (o *O3fsClient) String() string {
	return fmt.Sprintf("o3fs://%s/%s/%s", o.addr, o.volume, o.bucket)
}

func (o *O3fsClient) path(key string) string {
	return "/" + key
}

func (o *O3fsClient) Head(key string) (Object, error) {
	info, err := o.ozClient.OmClient.LookupFile(o.volume, o.bucket, key)
	if err != nil {
		return nil, err
	}

	f := &file{
		obj{
			key,
			int64(*info.DataSize),
			time.UnixMilli(int64(*info.ModificationTime)),
			false, // TODO:
		},
		superuser,   // TODO:
		supergroup,  // TODO:
		os.ModePerm, // TODO:
		false,
	}
	if f.owner == superuser {
		f.owner = "root"
	}
	if f.group == supergroup {
		f.group = "root"
	}
	// stickybit from HDFS is different than golang
	if f.mode&01000 != 0 {
		f.mode &= ^os.FileMode(01000)
		f.mode |= os.ModeSticky
	}
	if false { // TODO: info.IsDir() {
		f.size = 0
		if !strings.HasSuffix(f.key, "/") {
			f.key += "/"
		}
	}
	return f, nil
}

func (o *O3fsClient) Get(key string, off, limit int64) (io.ReadCloser, error) {
	f, err := OpenO3file(o, key)
	if err != nil {
		return nil, err
	}

	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	if limit > 0 {
		return withCloser{io.LimitReader(f, limit), f}, nil
	}
	return f, nil
}

func (o *O3fsClient) Put(key string, in io.Reader) error {
	_, err := o.ozClient.PutKey(o.volume, o.bucket, key, in)
	return err
}

func (o *O3fsClient) Delete(key string) error {
	return o.ozClient.DeleteKey(o.volume, o.bucket, key)
}

func (h *O3fsClient) List(prefix, marker string, limit int64) ([]Object, error) {
	return nil, notSupported
}

func (o *O3fsClient) walk(path string, fi fs.FileInfo, walkFn filepath.WalkFunc) error {
	if strings.HasSuffix(path, "/") {
		infos, err := o.ozClient.ListStatus(o.volume, o.bucket, strings.TrimPrefix(path, "/"))
		if err != nil {
			return walkFn(path, nil, err)
		}
		// make sure they are ordered in full path
		names := make([]string, len(infos))
		for i, info := range infos {
			if err != nil {
				return err
			}
			if info.IsDir() {
				names[i] = info.Name() + "/"
			} else {
				names[i] = info.Name()
			}
		}
		sort.Strings(names)

		for _, name := range names {
			name = strings.TrimSuffix(name, "/")
			err = o.walk(filepath.ToSlash(filepath.Join(path, name)), fi, walkFn)
			if err != nil {
				return err
			}
		}
		return nil
	}
	if fi != nil {
		walkFn(filepath.ToSlash(filepath.Join(path, fi.Name())), fi, nil)
	}

	return nil
}

func (o *O3fsClient) ListAll(prefix, marker string) (<-chan Object, error) {
	listed := make(chan Object, 10240)
	root := o.path(prefix)
	_, err := o.ozClient.InfoFile(o.volume, o.bucket, root)
	if err != nil || !strings.HasSuffix(prefix, "/") {
		root = filepath.Dir(root)
	}
	_, err = o.ozClient.InfoFile(o.volume, o.bucket, root)
	if err != nil {
		close(listed)
		return listed, nil // return empty list
	}
	go func() {
		_ = o.walk(root, nil, func(path string, info fs.FileInfo, err error) error {
			if err != nil {
				if err == io.EOF {
					err = nil // ignore
				} else {
					logger.Errorf("list %s: %s", path, err)
					listed <- nil
				}
				return err
			}
			key := path[1:]
			if !strings.HasPrefix(key, prefix) || key < marker {
				if info.IsDir() && !strings.HasPrefix(prefix, key) && !strings.HasPrefix(marker, key) {
					return filepath.SkipDir
				}
				return nil
			}
			oinfo := info.(*api.O3fsFileInfo)
			f := &file{
				obj{
					key,
					info.Size(),
					info.ModTime(),
					info.IsDir(),
				},
				oinfo.Owner(),
				oinfo.OwnerGroup(),
				info.Mode(),
				false,
			}
			if f.owner == superuser {
				f.owner = "root"
			}
			if f.group == supergroup {
				f.group = "root"
			}
			// stickybit from HDFS is different than golang
			if f.mode&01000 != 0 {
				f.mode &= ^os.FileMode(01000)
				f.mode |= os.ModeSticky
			}
			if info.IsDir() {
				f.size = 0
				if path != root || !strings.HasSuffix(root, "/") {
					f.key += "/"
				}
			}
			listed <- f
			return nil
		})
		close(listed)
	}()
	return listed, nil
}

func (h *O3fsClient) Chtimes(key string, mtime time.Time) error {
	return nil
	//return h.c.Chtimes(h.path(key), mtime, mtime)
}

func (h *O3fsClient) Chmod(key string, mode os.FileMode) error {
	return nil
	//return h.c.Chmod(h.path(key), mode)
}

func (h *O3fsClient) Chown(key string, owner, group string) error {
	if owner == "root" {
		owner = superuser
	}
	if group == "root" {
		group = supergroup
	}
	return nil
	//return h.c.Chown(h.path(key), owner, group)
}

func newOzoneFS(endpoint, username, sk, token string) (ObjectStorage, error) {
	if !strings.Contains(endpoint, "://") {
		endpoint = fmt.Sprintf("o3fs://%s", endpoint)
	}
	endpoint = strings.Trim(endpoint, "/")
	uri, err := url.ParseRequestURI(endpoint)
	if err != nil {
		return nil, fmt.Errorf("Invalid endpoint %s: %s", endpoint, err.Error())
	}

	var (
		volumeName string
		bucketName string
		omAddress  string
	)

	if uri.Path != "" {
		// [ENDPOINT]/[BUCKET]
		pathParts := strings.Split(uri.Path, "/")
		volumeName = pathParts[1]
		bucketName = pathParts[2]
		omAddress = uri.Host
	}
	conf, err := hadoopconf.LoadFromEnvironment()
	if err != nil {
		return nil, fmt.Errorf("Problem loading configuration: %s", err)
	}

	options := hdfs.ClientOptionsFromConf(conf)

	if options.KerberosClient != nil {
		options.KerberosClient, err = getKerberosClient()
		if err != nil {
			return nil, fmt.Errorf("Problem with kerberos authentication: %s", err)
		}
	} else {
		if username == "" {
			username = os.Getenv("HADOOP_USER_NAME")
		}
		if username == "" {
			current, err := user.Current()
			if err != nil {
				return nil, fmt.Errorf("get current user: %s", err)
			}
			username = current.Username
		}
		options.User = username
	}

	c := api.CreateOzoneClient(omAddress)
	if err != nil {
		return nil, fmt.Errorf("new Ozone client %s: %s", omAddress, err)
	}
	if os.Getenv("HADOOP_SUPER_USER") != "" {
		superuser = os.Getenv("HADOOP_SUPER_USER")
	}
	if os.Getenv("HADOOP_SUPER_GROUP") != "" {
		supergroup = os.Getenv("HADOOP_SUPER_GROUP")
	}

	return &O3fsClient{addr: omAddress, volume: volumeName, bucket: bucketName, ozClient: c}, nil
}

func init() {
	Register("o3fs", newOzoneFS)
}
