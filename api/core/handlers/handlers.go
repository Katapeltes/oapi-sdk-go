package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/larksuite/oapi-sdk-go/api/core/constants"
	"github.com/larksuite/oapi-sdk-go/api/core/errors"
	"github.com/larksuite/oapi-sdk-go/api/core/request"
	"github.com/larksuite/oapi-sdk-go/api/core/response"
	"github.com/larksuite/oapi-sdk-go/api/core/token"
	"github.com/larksuite/oapi-sdk-go/api/core/transport"
	"github.com/larksuite/oapi-sdk-go/core"
	"github.com/larksuite/oapi-sdk-go/core/config"
	coreconst "github.com/larksuite/oapi-sdk-go/core/constants"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"os"
	"reflect"
	"strings"
)

const defaultMaxRetryCount = 1

var defaultHTTPRequestHeader = map[string]string{}
var Default = &Handlers{}

func init() {
	defaultHTTPRequestHeader["User-Agent"] = fmt.Sprintf("oapi-sdk-go/%s", core.SdkVersion)
	Default.init = initFunc
	Default.validate = validateFunc
	Default.build = buildFunc
	Default.sign = signFunc
	Default.unmarshalResponse = unmarshalResponseFunc
	Default.validateResponse = validateResponseFunc
	Default.retry = retryFunc
	Default.complement = complementFunc
}

type Handler func(*core.Context, *request.Request)

type Handlers struct {
	init              Handler
	validate          Handler
	build             Handler // build http request
	sign              Handler // sign token to header
	validateResponse  Handler
	unmarshalResponse Handler
	retry             Handler // when token invalid, retry
	complement        Handler
}

func Handle(ctx *core.Context, req *request.Request) {
	defer Default.complement(ctx, req)
	Default.init(ctx, req)
	if req.Err != nil {
		return
	}
	Default.validate(ctx, req)
	if req.Err != nil {
		return
	}
	i := 0
	for {
		i++
		Default.send(ctx, req)
		if !req.Retryable || i > defaultMaxRetryCount {
			return
		}
		config.ByCtx(ctx).GetLogger().Debug(ctx, fmt.Sprintf("[retry] request:%v, err: %v", req, req.Err))
		req.Err = nil
	}
}

func (hs *Handlers) send(ctx *core.Context, req *request.Request) {
	hs.build(ctx, req)
	if req.Err != nil {
		return
	}
	hs.sign(ctx, req)
	if req.Err != nil {
		return
	}
	resp, err := transport.DefaultClient.Do(req.HTTPRequest)
	if err != nil {
		req.Err = err
		return
	}
	ctx.Set(coreconst.HTTPHeaderKeyRequestID, resp.Header.Get(coreconst.HTTPHeaderKeyRequestID))
	ctx.Set(coreconst.HTTPKeyStatusCode, resp.StatusCode)
	req.HTTPResponse = resp
	defer hs.retry(ctx, req)
	hs.validateResponse(ctx, req)
	if req.Err != nil {
		return
	}
	hs.unmarshalResponse(ctx, req)
}

func initFunc(_ *core.Context, req *request.Request) {
	req.Err = req.Init()
}

func validateFunc(ctx *core.Context, req *request.Request) {
	if req.AccessTokenType == request.AccessTokenTypeNone {
		return
	}
	if _, ok := req.AccessibleTokenTypeSet[req.AccessTokenType]; !ok {
		req.Err = errors.ErrAccessTokenTypeIsInValid
	}
	if config.ByCtx(ctx).GetAppSettings().AppType == coreconst.AppTypeISV {
		if req.AccessTokenType == request.AccessTokenTypeTenant && req.TenantKey == "" {
			req.Err = errors.ErrTenantKeyIsEmpty
			return
		}
		if req.AccessTokenType == request.AccessTokenTypeUser && req.UserAccessToken == "" {
			req.Err = errors.ErrUserAccessTokenKeyIsEmpty
			return
		}
	}
}

func buildFunc(ctx *core.Context, req *request.Request) {
	conf := config.ByCtx(ctx)
	if !req.Retryable {
		if req.Input != nil {
			switch req.Input.(type) {
			case *request.FormData:
				reqBodyFromFormData(ctx, req)
				conf.GetLogger().Debug(ctx, fmt.Sprintf("[build]request:\n%v\nbody:formdata", req))
			default:
				reqBodyFromInput(ctx, req)
				conf.GetLogger().Debug(ctx, fmt.Sprintf("[build]request:\n%v\nbody:%s", req, string(req.RequestBody)))
			}
		}
		if req.Err != nil {
			return
		}
	}
	if req.RequestBody != nil {
		req.RequestBodyStream = bytes.NewBuffer(req.RequestBody)
	}
	if err := requestBodyStream(req); err != nil {
		req.Err = err
		return
	}
	r, err := http.NewRequestWithContext(ctx, req.HttpMethod, req.FullUrl(conf.GetDomain()), req.RequestBodyStream)
	if err != nil {
		req.Err = err
		return
	}
	for k, v := range defaultHTTPRequestHeader {
		r.Header.Set(k, v)
	}
	r.Header.Set(coreconst.ContentType, req.ContentType)
	req.HTTPRequest = r
}

func requestBodyStream(req *request.Request) error {
	var err error
	if seek, ok := req.RequestBodyStream.(io.Seeker); ok {
		_, err = seek.Seek(0, 0)
		if err != nil {
			if pathError, ok := err.(*os.PathError); ok {
				if pathError.Err == os.ErrClosed {
					if file, ok := seek.(*os.File); ok {
						req.RequestBodyStream, err = os.Open(file.Name())
					}
				}
			}
		}
	}
	return err
}

func signFunc(ctx *core.Context, req *request.Request) {
	var httpRequest *http.Request
	var err error
	switch req.AccessTokenType {
	case request.AccessTokenTypeApp:
		httpRequest, err = setAppAccessToken(ctx, req.HTTPRequest)
	case request.AccessTokenTypeTenant:
		httpRequest, err = setTenantAccessToken(ctx, req.HTTPRequest)
	case request.AccessTokenTypeUser:
		httpRequest, err = setUserAccessToken(ctx, req.HTTPRequest)
	default:
		httpRequest, err = req.HTTPRequest, req.Err
	}
	req.HTTPRequest = httpRequest
	req.Err = err
}

func validateResponseFunc(_ *core.Context, req *request.Request) {
	resp := req.HTTPResponse
	if req.IsResponseStream {
		if resp.StatusCode != http.StatusOK {
			req.Err = fmt.Errorf("response is stream, but status code:%d", resp.StatusCode)
		}
		return
	}
	contentTypes := resp.Header[coreconst.ContentType]
	if len(contentTypes) == 0 || !strings.Contains(contentTypes[0], coreconst.ContentTypeJson) {
		respBody, err := readResponse(resp)
		if err != nil {
			req.Err = err
			return
		}
		req.Err = response.NewErrorOfInvalidResp(fmt.Sprintf("content-type: %s, is not: %s, if is stream, "+
			"please `request.SetIsResponseStream()`, body:%s", contentTypes[0], coreconst.ContentTypeJson, string(respBody)))
	}
}

func unmarshalResponseFunc(ctx *core.Context, req *request.Request) {
	resp := req.HTTPResponse
	if req.IsResponseStream {
		defer resp.Body.Close()
		switch output := req.Output.(type) {
		case io.Writer:
			_, err := io.Copy(output, resp.Body)
			if err != nil {
				req.Err = err
				return
			}
		default:
			req.Err = fmt.Errorf("request`s Output type must implement `io.Writer` interface")
			return
		}
		return
	}
	respBody, err := readResponse(resp)
	if err != nil {
		req.Err = err
		return
	}
	config.ByCtx(ctx).GetLogger().Debug(ctx, fmt.Sprintf("[unmarshalResponse] request:%v\nresponse:\nbody:%s",
		req, string(respBody)))
	if req.DataFilled() {
		err := unmarshalJSON(req.Output, req.IsNotDataField, bytes.NewBuffer(respBody))
		if err != nil {
			req.Err = err
			return
		}
	} else {
		req.Err = fmt.Errorf("request out do not write")
		return
	}
}

func readResponse(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return respBody, nil
}

func retryFunc(_ *core.Context, req *request.Request) {
	if req.Err != nil {
		if err, ok := req.Err.(*response.Error); ok {
			req.Info.Retryable = err.Retryable()
			return
		}
	}
	req.Info.Retryable = false
}

func complementFunc(ctx *core.Context, req *request.Request) {
	switch err := req.Err.(type) {
	case *response.Error:
		switch err.Code {
		case response.ErrCodeAppTicketInvalid:
			applyAppTicket(ctx)
		}
	default:
		if req.Err == errors.ErrAppTicketIsEmpty {
			applyAppTicket(ctx)
		}
	}
}

// apply app ticket
func applyAppTicket(ctx *core.Context) {
	conf := config.ByCtx(ctx)
	req := request.NewRequestByAuth(constants.ApplyAppTicketPath, http.MethodPost,
		&token.ApplyAppTicketReq{
			AppID:     conf.GetAppSettings().AppID,
			AppSecret: conf.GetAppSettings().AppSecret,
		}, &response.NoData{})
	Handle(ctx, req)
	if req.Err != nil {
		conf.GetLogger().Error(ctx, req.Err)
	}
}

func unmarshalJSON(v interface{}, isNotDataField bool, stream io.Reader) error {
	var e response.Error
	if isNotDataField {
		typ := reflect.TypeOf(v)
		name := typ.Elem().Name()
		responseTyp := reflect.StructOf([]reflect.StructField{
			{
				Name:      "Error",
				Anonymous: true,
				Type:      reflect.TypeOf(response.Error{}),
			},
			{
				Name:      name,
				Anonymous: true,
				Type:      typ,
			},
		})
		responseV := reflect.New(responseTyp).Elem()
		responseV.Field(1).Set(reflect.ValueOf(v))
		s := responseV.Addr().Interface()
		err := json.NewDecoder(stream).Decode(s)
		if err != nil {
			return err
		}
		e = responseV.Field(0).Interface().(response.Error)
	} else {
		out := &response.Response{
			Data: v,
		}
		err := json.NewDecoder(stream).Decode(&out)
		if err != nil {
			return err
		}
		e = out.Error
	}
	if e.Code == response.ErrCodeOk {
		return nil
	}
	return &e
}

func reqBodyFromFormData(_ *core.Context, req *request.Request) {
	var reqBody io.ReadWriter
	fd := req.Input.(*request.FormData)
	hasStream := fd.HasStream()
	if hasStream {
		reqBody, req.Err = ioutil.TempFile("", ".larksuiteoapisdk")
		if req.Err != nil {
			return
		}
	} else {
		reqBody = &bytes.Buffer{}
	}
	writer := multipart.NewWriter(reqBody)
	for key, val := range fd.Params() {
		err := writer.WriteField(key, fmt.Sprint(val))
		if err != nil {
			req.Err = err
			return
		}
	}
	for _, file := range fd.Files() {
		part, err := writer.CreatePart(file.MIMEHeader())
		if err != nil {
			req.Err = err
			return
		}
		_, err = io.Copy(part, file)
		if err != nil {
			req.Err = err
			return
		}
	}
	req.ContentType = writer.FormDataContentType()
	err := writer.Close()
	if err != nil {
		req.Err = err
		return
	}
	if hasStream {
		req.RequestBodyStream = reqBody
	} else {
		req.RequestBody, req.Err = ioutil.ReadAll(reqBody)
	}
}

func reqBodyFromInput(_ *core.Context, req *request.Request) {
	var bs []byte
	if input, ok := req.Input.(string); ok {
		bs = []byte(input)
	} else {
		reqBody := new(bytes.Buffer)
		err := json.NewEncoder(reqBody).Encode(req.Input)
		if err != nil {
			req.Err = err
			return
		}
		bs = reqBody.Bytes()
	}
	req.ContentType = coreconst.DefaultContentType
	req.RequestBody = bs
}