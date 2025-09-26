package pan123

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	limiter *rate.Limiter
}

const (
	rootUrl = "https://open-api.123pan.com"
)

// Globals
var (
	preRefreshDuration = time.Duration(5 * time.Minute)

	apiUserInfo    = Api{"api/v1/user/info", rate.NewLimiter(rate.Limit(1), 1)}
	apiAccessToken = Api{"api/v1/access_token", rate.NewLimiter(rate.Limit(1), 1)}
	apiFileMove    = Api{"api/v1/file/move", rate.NewLimiter(rate.Limit(1), 1)}
	apiFileDelete  = Api{"api/v1/file/delete", rate.NewLimiter(rate.Limit(1), 1)}
	apiFileList    = Api{"api/v1/file/list", rate.NewLimiter(rate.Limit(4), 4)}
	apiMkdir       = Api{"upload/v1/file/mkdir", rate.NewLimiter(rate.Limit(2), 2)}
	apiFileCreate  = Api{"upload/v1/file/create", rate.NewLimiter(rate.Limit(2), 2)}
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
	if f.opt.accessToken != "" && f.opt.expiredAt != "" {
		parsedTime, err := time.Parse(time.RFC3339, f.opt.expiredAt)
		if err != nil {
			return err
		}
		if time.Now().Add(preRefreshDuration).Before(parsedTime) {
			return nil
		}
	}

	opts := rest.Opts{
		Method: "POST",
		Path:   apiAccessToken.uri,
	}
	var request = api.GetAccessToken{
		ClientId:     f.opt.clientId,
		ClientSecret: f.opt.clientSecret,
	}

	apiAccessToken.limiter.Wait(ctx)
	resp := api.ApiResponse[api.GetAccessTokenResponse]{}
	_, err := f.srv.CallJSON(ctx, &opts, &request, &resp)
	if err != nil {
		return err
	}

	if resp.Code != 200 {
		return fmt.Errorf("failed to get access token: %s", resp.Message)
	}

	f.srv.SetHeader("Authorization", "Bearer "+resp.Data.AccessToken)
	f.opt.accessToken = resp.Data.AccessToken
	f.opt.expiredAt = resp.Data.ExpiredAt
	return nil
}

type Fs struct {
	name     string             // name of this remote
	root     string             // the path we are working on
	opt      Options            // parsed options
	features *fs.Features       // optional features
	srv      *rest.Client       // the connection to the server
	dirCache *dircache.DirCache // Map of directory path to directory id
}

type Options struct {
	clientId     string `config:"client_id"`
	clientSecret string `config:"client_secret"`
	accessToken  string `config:"access_token"`
	expiredAt    string `config:"expired_at"`
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

// Returns the supported hash types of the filesystem
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.MD5)
}

// parsePath parses a box 'url'
func parsePath(path string) (root string) {
	root = strings.Trim(path, "/")
	return
}

// errorHandler parses a non 2xx error response into an error
func errorHandler(resp *http.Response) error {
	// Decode error response
	errResponse := new(api.ApiResponse[string])
	err := rest.DecodeJSON(resp, &errResponse)
	if err != nil {
		fs.Debugf(nil, "Couldn't decode error response: %v", err)
	}
	if errResponse.Code == 0 {
		errResponse.Code = resp.StatusCode
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

	if opt.clientId == "" || opt.clientSecret == "" {
		return nil, errors.New("client_id and client_secret are required")
	}

	root = strings.Trim(root, "/")
	client := fshttp.NewClient(ctx)
	srv := rest.NewClient(client).SetRoot(rootUrl)
	srv.SetHeader("Platform", "open_platform")
	srv.SetHeader("Content-Type", "application/json")

	// TODO: use error handler to process reauth
	f := &Fs{
		name: name,
		root: root,
		opt:  *opt,
		srv:  srv,
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

	return f, nil
}

func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	return nil, fs.ErrorNotImplemented
}

func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	return nil, fs.ErrorNotImplemented
}

func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return nil, fs.ErrorNotImplemented
}

func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	return fs.ErrorNotImplemented
}

func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	return fs.ErrorNotImplemented
}
