package registry

import (
	"context"
	"time"

	L "github.com/ntons/libra-go"
	v1pb "github.com/ntons/libra-go/api/libra/v1"
	log "github.com/ntons/log-go"
)

type userAdminServer struct {
	v1pb.UnimplementedUserAdminServer
}

func newUserAdminServer() *userAdminServer {
	return &userAdminServer{}
}

func (srv *userAdminServer) SetMetadata(
	ctx context.Context, req *v1pb.UserAdminSetMetadataRequest) (
	_ *v1pb.UserAdminSetMetadataResponse, err error) {
	trusted := L.RequireAuthBySecret(ctx)
	if trusted == nil || !idBelongToAppId(trusted.AppId, req.UserId) {
		return nil, errUnauthenticated
	}
	if err = setUserMetadata(
		ctx, trusted.AppId, req.UserId, req.Metadata); err != nil {
		log.Warnf("failed to set user metadata: %v", err)
		return
	}
	return &v1pb.UserAdminSetMetadataResponse{}, nil
}

func (srv *userAdminServer) GetMetadata(
	ctx context.Context, req *v1pb.UserAdminGetMetadataRequest) (
	_ *v1pb.UserAdminGetMetadataResponse, err error) {
	trusted := L.RequireAuthBySecret(ctx)
	if trusted == nil || !idBelongToAppId(trusted.AppId, req.UserId) {
		return nil, errUnauthenticated
	}
	user, err := getUser(ctx, trusted.AppId, req.UserId)
	if err != nil {
		log.Warnf("failed to get user: %v", err)
		return
	}
	return &v1pb.UserAdminGetMetadataResponse{
		Metadata: user.Metadata,
	}, nil
}

func (srv *userAdminServer) Get(
	ctx context.Context, req *v1pb.UserAdminGetRequest) (
	_ *v1pb.UserAdminGetResponse, err error) {
	trusted := L.RequireAuthBySecret(ctx)
	if trusted == nil || !idBelongToAppId(trusted.AppId, req.Ids...) {
		return nil, errUnauthenticated
	}
	var (
		userIds = req.Ids
		roleIds []string
	)
	if req.Options != nil && req.Options.Fuzzy {
		userIds = make([]string, 0, len(req.Ids))
		roleIds = make([]string, 0, len(req.Ids))
		for _, id := range req.Ids {
			if _, tag, _ := decId(id); tag == userIdTag {
				userIds = append(userIds, id)
			} else if tag == roleIdTag {
				roleIds = append(roleIds, id)
			}
		}
	}
	if len(roleIds) > 0 {
		roles, err := getRoles(ctx, trusted.AppId, roleIds)
		if err != nil {
			log.Warnf("failed to get roles: %v", err)
			return nil, errDatabaseUnavailable
		}
		for _, role := range roles {
			userIds = append(userIds, role.UserId)
		}
	}
	var (
		users []*dbUser
		roles []*dbRole
	)
	if users, err = getUsers(ctx, trusted.AppId, userIds); err != nil {
		log.Warnf("failed to get users: %v", err)
		return nil, errDatabaseUnavailable
	}
	if req.Options != nil && req.Options.WithRoles {
		if roles, err = getRolesByUserId(
			ctx, trusted.AppId, userIds); err != nil {
			log.Warnf("failed to get roles: %v", err)
			return nil, errDatabaseUnavailable
		}
	}
	return &v1pb.UserAdminGetResponse{
		Users: fromDbUsers(users),
		Roles: fromDbRoles(roles),
	}, nil
}

func (srv *userAdminServer) Ban(
	ctx context.Context, req *v1pb.UserAdminBanRequest) (
	_ *v1pb.UserAdminBanResponse, err error) {
	trusted := L.RequireAuthBySecret(ctx)
	if trusted == nil || !idBelongToAppId(trusted.AppId, req.UserIds...) {
		return nil, errUnauthenticated
	}
	res := &v1pb.UserAdminBanResponse{}
	if len(req.UserIds) > 0 {
		if req.Seconds > 0 {
			// ban
			if err = banUsers(
				ctx,
				trusted.AppId,
				req.UserIds,
				time.Now().Add(time.Duration(req.Seconds)*time.Second),
				req.Reason,
			); err != nil {
				log.Warnf("failed to ban users: %v", err)
				return nil, errDatabaseUnavailable
			}
			if err = logoutUser(ctx, req.UserIds...); err != nil {
				log.Warnf("failed to logout users: %v", err)
				return nil, errDatabaseUnavailable
			}
		} else if req.Seconds < 0 {
			// unban
			if err = unbanUsers(
				ctx,
				trusted.AppId,
				req.UserIds,
			); err != nil {
				log.Warnf("failed to unban users: %v", err)
				return nil, errDatabaseUnavailable
			}
		}
		users, err := getUsersWithFields(
			ctx, trusted.AppId, req.UserIds, "ban_to", "ban_for")
		if err != nil {
			log.Warnf("failed to get users: %v", err)
			return nil, errDatabaseUnavailable
		}
		now := time.Now()
		for _, user := range users {
			state := &v1pb.UserBanState{Id: user.Id}
			if user.BanTo.After(now) {
				state.BanTo = user.BanTo.Unix()
				state.BanFor = user.BanFor
			}
			res.States = append(res.States, state)
		}
	}
	return res, nil
}

func (srv *userAdminServer) BindAcctId(
	ctx context.Context, req *v1pb.UserAdminBindAcctIdRequest) (
	_ *v1pb.UserAdminBindAcctIdResponse, err error) {
	trusted := L.RequireAuthBySecret(ctx)
	if trusted == nil || !idBelongToAppId(trusted.AppId, req.UserId) {
		return nil, errUnauthenticated
	}
	if _, err = bindAcctIdToUser(
		ctx, trusted.AppId, req.UserId, req.AcctIds,
		req.TakeOverAcctIdIfDuplicated,
	); err != nil {
		log.Warnf("failed to transfer acct id: %v", err)
		return nil, errDatabaseUnavailable
	}
	return &v1pb.UserAdminBindAcctIdResponse{}, nil
}