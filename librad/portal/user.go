package portal

import (
	"context"
	"strings"

	"github.com/ntons/libra-go/api/v1"
	log "github.com/ntons/log-go"
	"github.com/ntons/tongo/sign"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/ntons/libra/librad/comm"
)

func toUserData(x *xUser) *v1.UserData {
	return &v1.UserData{
		Id:       x.Id,
		AcctId:   x.AcctId,
		Metadata: x.Metadata,
	}
}
func toUserDataList(a []*xUser) []*v1.UserData {
	r := make([]*v1.UserData, 0, len(a))
	for _, x := range a {
		r = append(r, toUserData(x))
	}
	return r
}

type userServer struct {
	v1.UnimplementedUserServer
	comm.GrpcUnaryInterceptor
}

func newUserServer() *userServer {
	return &userServer{
		GrpcUnaryInterceptor: newTokenRequired("Login"),
	}
}

func (srv *userServer) checkState(
	app *xApp, any *anypb.Any) (acctId []string, err error) {
	state, err := anypb.UnmarshalNew(any, proto.UnmarshalOptions{})
	if err != nil {
		log.Warnf("failed to unmarshal state: %v", err)
		return nil, errInvalidState
	}
	switch state := state.(type) {
	case *v1.DevLoginState:
		acctId = []string{"dev$" + state.Username}
	case *v1.UniformLoginState:
		signature := state.Signature
		state.Signature = ""
		if !strings.EqualFold(
			signature, sign.ProtoHMACWithSHA1(state, app.Secret)) {
			return nil, errInvalidSignature
		}
		acctId = state.AcctId
	default:
		log.Warnf("unhandled state type: %T", state)
		return nil, errInvalidState
	}
	return
}

func (srv *userServer) Login(
	ctx context.Context, req *v1.UserLoginRequest) (
	resp *v1.UserLoginResponse, err error) {
	app := db.getAppById(req.AppId)
	if app == nil {
		log.Warnf("invalid app id: %v", req.AppId)
		return nil, errInvalidAppId
	}
	acctId, err := srv.checkState(app, req.State)
	if err != nil {
		log.Warnf("failed to check state: %v", err)
		return
	}
	user, err := db.loginUser(ctx, app, acctId)
	if err != nil {
		log.Warnf("failed to login user: %v", err)
		return
	}
	token, err := db.newToken(ctx, app, user.Id)
	if err != nil {
		log.Warnf("failed to new token: %v", err)
		return
	}
	grpc.SetHeader(ctx, metadata.Pairs(
		xLibraToken, token, xLibraCookieToken, token))
	return &v1.UserLoginResponse{User: toUserData(user)}, nil
}
func (srv *userServer) Bind(
	ctx context.Context, req *v1.UserBindRequest) (
	resp *v1.UserBindResponse, err error) {
	sess := getSessFromContext(ctx)
	if err = db.bindAcctIdToUser(
		ctx, sess.appId, sess.userId, req.AcctId); err != nil {
		log.Warnf("failed to bind acct to user: %v", err)
		return
	}
	return &v1.UserBindResponse{}, nil
}
func (srv *userServer) SetMetadata(
	ctx context.Context, req *v1.UserSetMetadataRequest) (
	resp *v1.UserSetMetadataResponse, err error) {
	sess := getSessFromContext(ctx)
	if err = db.setUserMetadata(
		ctx, sess.appId, sess.userId, req.Metadata); err != nil {
		log.Warnf("failed to set user metadata: %v", err)
		return
	}
	return &v1.UserSetMetadataResponse{}, nil
}
