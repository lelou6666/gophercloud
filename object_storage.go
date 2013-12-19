package gophercloud

import (
	"bytes"
	"fmt"
	"github.com/racker/perigee"
	"strings"
)

const (
	ContainerMetadataPrefix = "x-container-meta-"
)

// containerMetaName takes an unadorned custom metadata key and formats it suitably for map
// look-up.
func containerMetaName(s string) string {
	return strings.ToLower(ContainerMetadataPrefix + s)
}

// The openstackObjectStorageProvider structure provides the implementation for generic OpenStack-compatible
// object storage interfaces.
type openstackObjectStoreProvider struct {
	// endpoint refers to the provider's API endpoint base URL.  This will be used to construct
	// and issue queries.
	endpoint string

	// Test context (if any) in which to issue requests.
	context *Context

	// access associates this API provider with a set of credentials,
	// which may be automatically renewed if they near expiration.
	access AccessProvider
}

// openstackContainer provides the backing state required to keep track of a single container in an OpenStack
// environment.
type openstackContainer struct {
	// Name labels the container.
	Name string

	// Provider links the container to an actual provider.
	Provider *openstackObjectStoreProvider

	// customMetadata provides access to the custom metadata for this container.
	customMetadata *cimap
}

func (osp *openstackObjectStoreProvider) CreateContainer(name string) (Container, error) {
	var container Container

	err := osp.context.WithReauth(osp.access, func() error {
		url := osp.endpoint + "/" + name
		err := perigee.Put(url, perigee.Options{
			CustomClient: osp.context.httpClient,
			MoreHeaders: map[string]string{
				"X-Auth-Token": osp.access.AuthToken(),
			},
			OkCodes: []int{201},
		})
		if err == nil {
			container = &openstackContainer{
				Name:     name,
				Provider: osp,
			}
		}
		return err
	})
	return container, err
}

// See Container interface for details.
func (osp *openstackObjectStoreProvider) GetContainer(name string) Container {
	return &openstackContainer{
		Name:     name,
		Provider: osp,
	}
}

func (osp *openstackObjectStoreProvider) DeleteContainer(name string) error {
	err := osp.context.WithReauth(osp.access, func() error {
		url := osp.endpoint + "/" + name
		return perigee.Delete(url, perigee.Options{
			CustomClient: osp.context.httpClient,
			MoreHeaders: map[string]string{
				"X-Auth-Token": osp.access.AuthToken(),
			},
			OkCodes: []int{204},
		})
	})
	return err
}

func (c *openstackContainer) Delete() error {
	return c.Provider.DeleteContainer(c.Name)
}

func (c *openstackContainer) Metadata() (MetadataProvider, error) {
	// As of this writing, we let the openstackContainer structure keep track of its own metadata.
	return c, nil
}

// cacheHeaders() takes no action if custom metadata headers have already been retrieved.
// Otherwise, the container resource is queried for its current set of custom headers.
func (c *openstackContainer) cacheHeaders() error {
	osp := c.Provider
	return osp.context.WithReauth(osp.access, func() error {
		if c.customMetadata == nil {
			// Grab the set of headers attached to this container.
			// These headers will be keyed off of mixed-case strings.
			url := osp.endpoint + "/" + c.Name
			resp, err := perigee.Request("HEAD", url, perigee.Options{
				CustomClient: osp.context.httpClient,
				MoreHeaders: map[string]string{
					"X-Auth-Token": osp.access.AuthToken(),
				},
				OkCodes: []int{204},
			})
			if err != nil {
				return err
			}

			// To ensure case insensitivity when looking up keys,
			// transcribe our headers such that all the keys used to
			// index them are lower-case.
			headers := resp.HttpResponse.Header
			loweredHeaders := make(map[string]string)
			for key, values := range headers {
				key = strings.ToLower(key)
				if strings.HasPrefix(key, containerMetaName("")) {
					loweredHeaders[key[len(ContainerMetadataPrefix):]] = values[0]
				}
			}
			c.customMetadata = &cimap{m: loweredHeaders}
		}
		return nil
	})
}

// See MetadataProvider interface for details.
func (c *openstackContainer) CustomValues() (map[string]string, error) {
	err := c.cacheHeaders()
	if err != nil {
		return nil, err
	}
	return c.customMetadata.rawMap(), nil
}

// See MetadataProvider interface for details.
func (c *openstackContainer) CustomValue(key string) (string, error) {
	err := c.cacheHeaders()
	if err != nil {
		return "", err
	}
	key = strings.ToLower(key)
	value, _ := c.customMetadata.get(key)
	if len(value) > 0 {
		return value, nil
	}
	return "", nil
}

// See MetadataProvider interface for details.
func (c *openstackContainer) SetCustomValue(key, value string) error {
	osp := c.Provider
	err := osp.context.WithReauth(osp.access, func() error {
		url := osp.endpoint + "/" + c.Name
		_, err := perigee.Request("POST", url, perigee.Options{
			CustomClient: osp.context.httpClient,
			MoreHeaders: map[string]string{
				"X-Auth-Token":         osp.access.AuthToken(),
				containerMetaName(key): value,
			},
			OkCodes: []int{204},
		})
		return err
	})

	// Flush our values cache to make sure our next attempt at getting values always gets the right data.
	if err == nil {
		c.customMetadata = nil
	}

	return err
}

// See Container interface for details.
func (c *openstackContainer) BasicObjectDownloader(objOpts ObjectOpts) (*BasicDownloader, error) {
	bd := &BasicDownloader{}
	osp := c.Provider
	err := osp.context.WithReauth(osp.access, func() error {
		url := fmt.Sprintf("%s/%s/%s", osp.endpoint, c.Name, objOpts.Name)
		moreHeaders := map[string]string{
			"X-Auth-Token": osp.access.AuthToken(),
		}
		offset := objOpts.Offset
		length := objOpts.Length

		switch {
		case offset == 0 && length == 0:
			break
		case offset < 0 && length > 0:
			return fmt.Errorf("The provided offset-length combination is not supported: offset:%d, length:%d", offset, length)
		case offset < 0 && length == 0:
			moreHeaders["Range"] = fmt.Sprintf("bytes=%d", offset)
		case offset > 0 && length == 0:
			moreHeaders["Range"] = fmt.Sprintf("bytes=%d-", offset)
		default:
			moreHeaders["Range"] = fmt.Sprintf("bytes=%d-%d", offset, offset+length)
		}

		var res interface{}
		resp, err := perigee.Request("GET", url, perigee.Options{
			CustomClient: osp.context.httpClient,
			Results:      &res,
			MoreHeaders:  moreHeaders,
			OkCodes:      []int{200, 206},
		})
		fmt.Printf("resp.JsonResult: %+v\n", resp)
		bd.reader = bytes.NewReader(resp.JsonResult)

		return err
	})

	return bd, err
}

func (c *openstackContainer) BasicObjectUploader(objOpts ObjectOpts) (*BasicUploader, error) {
	b := make([]byte, 0)
	bu := &BasicUploader{
		container: c,
		name: objOpts.Name,
		buf: bytes.NewBuffer(b),
	}
	return bu, nil
}

// *BasicDownloader.Read uses the *bytes.Reader.Read method
func (bd *BasicDownloader) Read(p []byte) (int, error) {
	return bd.reader.Read(p)
}

// *BasicDownloader.Seek uses the *bytes.Reader.Seek method
func (bd *BasicDownloader) Seek(offset int64, whence int) (int64, error) {
	return bd.reader.Seek(offset, whence)
}

// *BasicDownloader.Close nil the reader, effectively "closing" it
func (bd *BasicDownloader) Close() error {
	bd.reader = nil
	return nil
}

// ObjectOpts is a structure containing relevant parameters when creating an uploader or downloader.
type ObjectOpts struct {
	Length int
	Name   string
	Offset int
}

func (bu *BasicUploader) Commit() error {
	c := bu.container.(*openstackContainer)
	osp := c.Provider
	err := osp.context.WithReauth(osp.access, func() error {
		url := fmt.Sprintf("%s/%s/%s", osp.endpoint, c.Name, bu.name)
		moreHeaders := map[string]string{
			"X-Auth-Token": osp.access.AuthToken(),
		}

		fmt.Printf("Length of buffer: %d\n", bu.buf.Len())

		reqBody := make([]byte, bu.buf.Len())

		n, err := bu.buf.Read(reqBody)

		if err != nil{
			return err
		}

		fmt.Printf("Number of bytes read: %d\n", n)
		fmt.Printf("reqBody (bytes): %v\n", reqBody)
		fmt.Printf("reqBody (string): %s\n", string(reqBody))

		_, err = perigee.Request("PUT", url, perigee.Options{
			CustomClient: osp.context.httpClient,
			ReqBody:	reqBody,
			//ReqBody:	string(reqBody),
			MoreHeaders:  moreHeaders,
			DumpReqJson: true,
			OkCodes:      []int{201},
		})

		return err
	})

	return err
}

func (bu *BasicUploader) Read(p []byte) (int, error) {
	return bu.buf.Read(p)
}

func (bu *BasicUploader) Write(p []byte) (int, error) {
	return bu.buf.Write(p)
}

func (bu *BasicUploader) Seek(offset int64, whence int) (int64, error){
	return 0, nil
}

func (bu *BasicUploader) Close() error {
	bu.buf = nil
	return nil
}

// BasicDownloader is a structure that embeds the *bytes.Reader structure. We use the Read and Seek methods of
// the *bytes.Reader for the corresponding BasicDownloader methods.
type BasicDownloader struct {
	reader *bytes.Reader
}

type BasicUploader struct {
	name	string
	container	Container
	buf *bytes.Buffer
}
