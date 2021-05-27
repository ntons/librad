package registry

import (
	"context"
	"strings"
	"time"

	v1pb "github.com/ntons/libra-go/api/v1"
	log "github.com/ntons/log-go"
	"github.com/ntons/tongo/sign"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/ntons/libra/librad/internal/comm"
)

func fromUser(x *xUser) *v1pb.UserData {
	return &v1pb.UserData{
		Id:       x.Id,
		AcctId:   x.AcctId,
		Metadata: x.Metadata,
	}
}

type userServer struct {
	v1pb.UnimplementedUserServer
}

func newUserServer() *userServer {
	return &userServer{}
}

func checkState(
	ctx context.Context, app *xApp, any *anypb.Any) (
	acctId []string, err error) {
	state, err := anypb.UnmarshalNew(any, proto.UnmarshalOptions{})
	if err != nil {
		log.Warnf("failed to unmarshal state: %v", err)
		return nil, errInvalidState
	}
	switch state := state.(type) {
	case *v1pb.DevLoginState:
		if !comm.IsDevEnv() {
			return nil, errInvalidState
		}
		acctId = []string{"dev$" + state.Username}
	case *v1pb.UniformLoginState:
		if ok, err := checkNonce(ctx, app.Id, state.Nonce); err != nil {
			return nil, errDatabaseUnavailable
		} else if !ok {
			return nil, errInvalidNonce
		}

		// ts-10 签名有效期只有10秒钟
		// ts+3  是为了容忍一定的系统时间误差
		ts := time.Now().Unix()
		if state.Timestamp < ts-10 || state.Timestamp > ts+3 {
			return nil, errInvalidTimestamp
		}

		signature := state.Signature
		state.Signature = ""
		expected := sign.ProtoHMACWithSHA1(state, app.Secret)
		if !strings.EqualFold(signature, expected) {
			log.Warnf("signature mismatch: %s, %s, %s, %s",
				signature, expected, app.Secret, state)
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
	ctx context.Context, req *v1pb.UserLoginRequest) (
	resp *v1pb.UserLoginResponse, err error) {
	app := getAppById(req.AppId)
	if app == nil {
		log.Warnf("invalid app id: %v", req.AppId)
		return nil, errInvalidAppId
	}
	acctId, err := checkState(ctx, app, req.State)
	if err != nil {
		log.Warnf("failed to check state: %v", err)
		return
	}
	user, sess, err := loginUser(ctx, app, acctId)
	if err != nil {
		log.Warnf("failed to login user: %v", err)
		return
	}
	grpc.SetHeader(ctx, metadata.Pairs(xLibraToken, sess.Token))
	return &v1pb.UserLoginResponse{User: fromUser(user)}, nil
}

func (srv *userServer) Bind(
	ctx context.Context, req *v1pb.UserBindRequest) (
	resp *v1pb.UserBindResponse, err error) {
	appId, userId, ok := getTrustedFromContext(ctx)
	if !ok {
		return nil, errLoginRequired
	}
	if err = bindAcctIdToUser(ctx, appId, userId, req.AcctId); err != nil {
		log.Warnf("failed to bind acct to user: %v", err)
		return
	}
	return &v1pb.UserBindResponse{}, nil
}

func (srv *userServer) SetMetadata(
	ctx context.Context, req *v1pb.UserSetMetadataRequest) (
	resp *v1pb.UserSetMetadataResponse, err error) {
	appId, userId, ok := getTrustedFromContext(ctx)
	if !ok {
		return nil, errLoginRequired
	}
	if err = setUserMetadata(ctx, appId, userId, req.Metadata); err != nil {
		log.Warnf("failed to set user metadata: %v", err)
		return
	}
	return &v1pb.UserSetMetadataResponse{}, nil
}