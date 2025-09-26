package pan123

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/backend/pan123/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/rest"
	"golang.org/x/time/rate"
)

type Api struct {
	uri     string
	method  string
	limiter *rate.Limiter
}

const (
	rootUrl        = "https://open-api.123pan.com"
	timeMetaLayout = "2006-01-02 15:04:05"
)

// Globals
var (
	preRefreshDuration = 7 * 24 * time.Hour

	apiUserInfo      = Api{"/api/v1/user/info", "", rate.NewLimiter(rate.Limit(1), 1)}
	apiAccessToken   = Api{"/api/v1/access_token", "POST", rate.NewLimiter(rate.Limit(1), 1)}
	apiFileMove      = Api{"/api/v1/file/move", "", rate.NewLimiter(rate.Limit(1), 1)}
	apiFileDelete    = Api{"/api/v1/file/delete", "", rate.NewLimiter(rate.Limit(1), 1)}
	apiFileList      = Api{"/api/v1/file/list", "GET", rate.NewLimiter(rate.Limit(4), 4)}
	apiFileInfoMulti = Api{"/api/v1/file/infos", "POST", rate.NewLimiter(rate.Limit(10), 10)}
	apiFileDownload  = Api{"/api/v1/file/download_info", "GET", rate.NewLimiter(rate.Limit(5), 5)}
	apiMkdir         = Api{"/upload/v1/file/mkdir", "", rate.NewLimiter(rate.Limit(2), 2)}
	apiFileCreate    = Api{"/upload/v1/file/create", "", rate.NewLimiter(rate.Limit(2), 2)}
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "pan123",
		Description: "123 Yun Pan",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:      "client_id",
			Help:      "pan123 open client_id",
			Required:  true,
			Sensitive: false,
		}, {
			Name:      "client_secret",
			Help:      "pan123 open client_secret",
			Required:  true,
			Sensitive: true,
		}, {
			Name:     "root_id",
			Help:     "pan123 root folder id",
			Default:  0,
			Required: true,
		}, {
			Name:     "access_token",
			Help:     "pan123 open access_token",
			Default:  "",
			Advanced: true,
		}, {
			Name:     "expired_at",
			Help:     "pan123 open expired_at",
			Default:  "",
			Advanced: true,
		}},
	})
}

func (f *Fs) authorizeAccount(ctx context.Context) error {
	if f.opt.AccessToken != "" && f.opt.ExpiredAt != "" {
		parsedTime, err := time.Parse(time.RFC3339, f.opt.ExpiredAt)
		if err != nil {
			return err
		}
		if time.Now().Add(preRefreshDuration).Before(parsedTime) {
			f.srv.SetHeader("Authorization", "Bearer "+f.opt.AccessToken)
			return nil
		}
	}

	opts := rest.Opts{
		Method: apiAccessToken.method,
		Path:   apiAccessToken.uri,
	}
	request := api.GetAccessToken{
		ClientId:     f.opt.ClientId,
		ClientSecret: f.opt.ClientSecret,
	}

	_ = apiAccessToken.limiter.Wait(ctx)
	resp := api.Response[api.GetAccessTokenResponse]{}
	_, err := f.srv.CallJSON(ctx, &opts, &request, &resp)
	if err != nil {
		return err
	}

	if resp.Code != 0 {
		return fmt.Errorf("failed to get access token: %d (%s)", resp.Code, resp.Message)
	}

	f.srv.SetHeader("Authorization", "Bearer "+resp.Data.AccessToken)
	f.opt.AccessToken = resp.Data.AccessToken
	f.opt.ExpiredAt = resp.Data.ExpiredAt
	f.m.Set("access_token", resp.Data.AccessToken)
	f.m.Set("expired_at", resp.Data.ExpiredAt)
	return nil
}

type Fs struct {
	name     string             // name of this remote
	root     string             // the path we are working on
	opt      Options            // parsed options
	features *fs.Features       // optional features
	srv      *rest.Client       // the connection to the server
	curl     *rest.Client       // for download
	dirCache *dircache.DirCache // Map of directory path to directory id
	m        configmap.Mapper   // configmap.Mapper
}

type Options struct {
	ClientId     string `config:"client_id"`
	ClientSecret string `config:"client_secret"`
	RootId       int64  `config:"root_id"`
	AccessToken  string `config:"access_token"`
	ExpiredAt    string `config:"expired_at"`
}

type Object struct {
	fs      *Fs       // what this object is part of
	remote  string    // The remote path
	size    int64     // size of the object
	modTime time.Time // modification time of the object
	id      string    // ID of the object
	md5     string    // MD5 of the object content
}

func (o *Object) Fs() fs.Info {
	return o.fs
}

func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

func (o *Object) Remote() string {
	return o.remote
}

func (o *Object) ModTime(ctx context.Context) time.Time {
	return o.modTime
}

func (o *Object) Size() int64 {
	return o.size
}

func (o *Object) Hash(ctx context.Context, ty hash.Type) (string, error) {
	if ty != hash.MD5 {
		return "", hash.ErrUnsupported
	}
	return o.md5, nil
}

func (o *Object) Storable() bool {
	return true
}

func (o *Object) SetModTime(ctx context.Context, t time.Time) error {
	return errors.New("SetModTime not supported on this backend")
}

func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	fs.FixRangeOption(options, o.size)

	if o.id == "" {
		return nil, errors.New("open object: can't download - no id")
	}

	opts := rest.Opts{
		Method: apiFileDownload.method,
		Path:   apiFileDownload.uri,
	}
	request := api.GetDownloadInfo{FileId: toId(o.id)}
	resp := api.Response[api.GetDownloadInfoResponse]{}

	_ = apiFileDownload.limiter.Wait(ctx)
	_, err := o.fs.srv.CallJSON(ctx, &opts, &request, &resp)
	if err != nil {
		return nil, err
	}

	if resp.Code != 0 {
		return nil, fmt.Errorf("failed to download: %d (%s)", resp.Code, resp.Message)
	}

	fs.Debugf(o, "download url: %s (%s) => %s", o.remote, o.id, resp.Data.DownloadUrl)
	opts.RootURL = resp.Data.DownloadUrl
	opts.Path = ""
	opts.Method = "GET"
	d, err := o.fs.curl.Call(ctx, &opts)
	if err != nil {
		return nil, err
	}
	return d.Body, err
}

func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	//TODO implement me
	panic("implement me")
}

func (o *Object) Remove(ctx context.Context) error {
	//TODO implement me
	panic("implement me")
}

// ------------------------------------------------------------

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String converts this Fs to a string
func (f *Fs) String() string {
	return fmt.Sprintf("pan123 root '%s'", f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// Precision returns the mod time of this Fs
func (f *Fs) Precision() time.Duration {
	return time.Millisecond
}

// Hashes Returns the supported hash types of the filesystem
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.MD5)
}

// parsePath parses a box 'url'
func parsePath(path string) (root string) {
	root = strings.Trim(path, "/")
	return
}

func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (pathIDOut string, found bool, err error) {
	//TODO implement me
	panic("implement me")
}

func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (newID string, err error) {
	//TODO implement me
	panic("implement me")
}

// errorHandler parses a non 2xx error response into an error
func errorHandler(resp *http.Response) error {
	// Decode error response
	errResponse := new(api.Response[string])
	err := rest.DecodeJSON(resp, &errResponse)
	if err != nil {
		fs.Debugf(nil, "Couldn't decode error response: %v", err)
	}
	return errResponse
}

// retryErrorCodes is a slice of error codes that we will retry
// TODO: fixme
var retryErrorCodes = []int{
	429, // Too Many Requests.
	500, // Internal Server Error
	502, // Bad Gateway
	503, // Service Unavailable
	504, // Gateway Timeout
	509, // Bandwidth Limit Exceeded
}

func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	// Parse config into Options struct
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	if opt.ClientId == "" || opt.ClientSecret == "" {
		return nil, errors.New("client_id and client_secret are required")
	}

	root = strings.Trim(root, "/")
	client := fshttp.NewClient(ctx)
	srv := rest.NewClient(client).SetRoot(rootUrl)
	srv.SetHeader("Platform", "open_platform")
	srv.SetHeader("Content-Type", "application/json")

	f := &Fs{
		name: name,
		root: root,
		opt:  *opt,
		srv:  srv,
		curl: rest.NewClient(client),
		m:    m,
	}

	f.features = (&fs.Features{
		CaseInsensitive:         true,
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)
	f.srv.SetErrorHandler(errorHandler)

	err = f.authorizeAccount(ctx)
	if err != nil {
		return nil, err
	}

	rootId := f.opt.RootId
	f.dirCache = dircache.New(root, toString(rootId), f)
	return f, nil
}

func (f *Fs) listAll(ctx context.Context, dirId int64) ([]api.CompleteFile, error) {
	files := make([]api.CompleteFile, 0)
	page := 1

	optsFile := rest.Opts{
		Method: apiFileList.method,
		Path:   apiFileList.uri,
	}
	optsMeta := rest.Opts{
		Method: apiFileInfoMulti.method,
		Path:   apiFileInfoMulti.uri,
	}
	requestFile := api.GetFileList{
		ParentFileId:   dirId,
		Page:           page,
		Limit:          100,
		OrderBy:        "file_name",
		OrderDirection: "asc",
		Trashed:        false,
	}
	requestMeta := api.GetFileInfoMulti{}
	respFile := api.Response[api.GetFileListResponse]{}
	respMeta := api.Response[api.GetFileInfoMultiResponse]{}

	for {
		fileMap := make(map[int64]api.File)

		_ = apiFileList.limiter.Wait(ctx)
		_, err := f.srv.CallJSON(ctx, &optsFile, &requestFile, &respFile)
		if err != nil {
			return nil, err
		}
		if respFile.Code != 0 {
			return nil, fmt.Errorf("failed to list files: %d (%s)", respFile.Code, respFile.Message)
		}

		requestMeta.Fields = make([]int64, 0)
		for _, file := range respFile.Data.FileList {
			fileMap[file.FileId] = file
			requestMeta.Fields = append(requestMeta.Fields, file.FileId)
		}

		_ = apiFileInfoMulti.limiter.Wait(ctx)
		_, err = f.srv.CallJSON(ctx, &optsMeta, &requestMeta, &respMeta)

		for _, meta := range respMeta.Data.FileList {
			cf := api.CompleteFile{}
			if file, ok := fileMap[meta.FileId]; ok {
				cf.File = file
			}
			cf.Meta = meta
			files = append(files, cf)
		}

		if len(respFile.Data.FileList) < 100 {
			break
		}
		page++
		requestFile.Page = page
	}

	return files, nil
}

func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	if dir != "" {
		return nil, errors.New("only root directories can be listed now")
	}

	list, err := f.listAll(ctx, f.opt.RootId)
	for _, cf := range list {
		remote := path.Join(dir, cf.File.FileName)
		if cf.File.Type == api.TypeFolder {
			f.dirCache.Put(remote, toString(cf.File.FileId))
			loc, _ := time.LoadLocation("Local")
			modTime, err := time.ParseInLocation(timeMetaLayout, cf.Meta.UpdateAt, loc)
			if err != nil {
				return nil, err
			}
			d := fs.NewDir(remote, modTime).SetID(toString(cf.File.FileId)).SetParentID(toString(f.opt.RootId))
			entries = append(entries, d)
		} else {
			o, err := f.NewObjectComplete(ctx, remote, cf)
			if err == nil {
				entries = append(entries, o)
			} else {
				fs.Debugf(nil, "Failed to list object: %v", err)
			}
		}
	}

	return entries, err
}

func (f *Fs) NewObjectComplete(ctx context.Context, remote string, cf api.CompleteFile) (fs.Object, error) {
	loc, _ := time.LoadLocation("Local")
	modTime, err := time.ParseInLocation(timeMetaLayout, cf.Meta.UpdateAt, loc)
	if err != nil {
		return nil, err
	}

	o := &Object{
		fs:      f,
		remote:  remote,
		size:    cf.File.Size,
		modTime: modTime,
		id:      toString(cf.File.FileId),
		md5:     cf.File.MD5,
	}

	return o, nil
}

func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	panic("not implemented yet")
}

func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	panic("not implemented yet")
}

func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	panic("not implemented yet")
}

func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	panic("not implemented yet")
}

func toString(x int64) string {
	return strconv.FormatInt(x, 10)
}

func toId(x string) int64 {
	id, err := strconv.ParseInt(x, 10, 64)
	if err != nil {
		fs.Errorf(nil, "Failed to parse id: %v", err)
		return 0
	}
	return id
}
