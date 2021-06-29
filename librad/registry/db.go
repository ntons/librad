package registry

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"time"

	"github.com/ntons/log-go"
	"github.com/vmihailenco/msgpack/v4"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/ntons/libra/librad/internal/redis"
	"github.com/ntons/libra/librad/internal/util"
)

// |- config |- apps
//
// |- app1   |- users
//           |- roles
//
// |- app2   |- users
//           |- roles

const (
	dbMaxAcctPerUser = 10
)

var (
	mdb *mongo.Client

	rdbAuth  redis.Client
	rdbNonce redis.Client

	// cached collection
	dbAppCollection  *mongo.Collection
	dbUserCollection = make(map[string]*mongo.Collection)
	dbRoleCollection = make(map[string]*mongo.Collection)

	// app cache loaded from database
	xApps = newAppIndex(nil)

	luaUpdateSessData = redis.NewScript(`
local b = redis.call("GET", KEYS[1])
if not b then return Nil end
local d = cmsgpack.unpack(b)
d.data = cmsgpack.unpack(ARGV[1])
return redis.call("SET", KEYS[1], cmsgpack.pack(d))`)
)

type xApp struct {
	// 应用ID
	Id string `bson:"_id"`
	// 数值形式的应用ID
	Key uint32 `bson:"key"`
	// 应用签名密钥，授权访问
	Secret string `bson:"secret,omitempty"`
	// 应用指纹指纹，特异化应用数据，增加安全性
	Fingerprint string `bson:"fingerprint,omitempty"`
	// 允许的服务
	Permissions []*xPermission `bson:"permissions,omitempty"`
	// AES密钥，由Fingerprint生成
	block cipher.Block
}

func (x *xApp) parse() (err error) {
	// check permission expression
	for _, p := range x.Permissions {
		if err = p.parse(); err != nil {
			return
		}
	}
	// hash fingerprint to 32 bytes byte array, NewCipher must success
	hash := sha256.Sum256([]byte(x.Fingerprint))
	x.block, _ = aes.NewCipher(hash[:])
	return
}
func (x *xApp) isPermitted(path string) bool {
	for _, p := range cfg.CommonPermissions {
		if p.isPermitted(path) {
			return true
		}
	}
	for _, p := range x.Permissions {
		if p.isPermitted(path) {
			return true
		}
	}
	return false
}

// App collection with index
type xAppIndex struct {
	idIndex  map[string]*xApp
	keyIndex map[uint32]*xApp
}

func newAppIndex(apps []*xApp) *xAppIndex {
	var (
		idIndex  = make(map[string]*xApp)
		keyIndex = make(map[uint32]*xApp)
	)
	for _, a := range apps {
		idIndex[a.Id] = a
		keyIndex[a.Key] = a
	}
	return &xAppIndex{idIndex: idIndex, keyIndex: keyIndex}
}
func (x xAppIndex) findById(id string) *xApp {
	a, _ := x.idIndex[id]
	return a
}
func (x xAppIndex) findByKey(key uint32) *xApp {
	a, _ := x.keyIndex[key]
	return a
}

// 会话缓存数据
type xSessData struct {
	RoleId    string `msgpack:"roleId"`
	RoleIndex uint32 `msgpack:"roleIndex"`
}
type xSess struct {
	AppId  string    `msgpack:"-"`
	UserId string    `msgpack:"-"`
	Token  string    `msgpack:"token"`
	Data   xSessData `msgpack:"data"`
	//// 中转数据
	app *xApp `msgpack:"-"`
}

type dbUser struct {
	Id string `bson:"_id"`
	// 用户账号列表，其中任意一个匹配都可以认定为该用户
	// 常见的用例为：
	// 1. 游客账号/正式账号
	// 2. 平台账号/第三方账号
	AcctIds []string `bson:"acct_ids,omitempty"`
	// 创建时间
	CreateTime time.Time `bson:"create_time,omitempty"`
	// 创建时IP
	CreateIp string `bson:"create_ip,omitempty"`
	// 上次登录时间
	LoginTime time.Time `bson:"login_time,omitempty"`
	// 上次登录时IP
	LoginIp string `bson:"login_ip,omitempty"`
	// 元数据
	Metadata map[string]string `bson:"metadata,omitempty"`
}

type dbRole struct {
	Id string `bson:"_id"`
	// 角色序号，主要有一下几个用途
	// 1. 创建用户发生重试时保证只有唯一一个角色被成功创建
	// 2. 用来确定该用户的角色分类，比如分区分服
	Index uint32 `bson:"index,omitempty"`
	// 所属用户ID
	UserId string `bson:"user_id,omitempty"`
	// 创建时间
	CreateTime time.Time `bson:"create_time,omitempty"`
	// 上次登录时间
	SignInTime time.Time `bson:"sign_in_time,omitempty"`
	// 元数据
	Metadata map[string]string `bson:"metadata,omitempty"`
}

func dialMongo(ctx context.Context) (_ *mongo.Client, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cli, err := mongo.NewClient(options.Client().ApplyURI(cfg.Mongo))
	if err != nil {
		return nil, fmt.Errorf("failed to new mongo client: %w", err)
	}
	if err = cli.Connect(ctx); err != nil {
		return nil, fmt.Errorf("failed to connect mongo: %w", err)
	}
	return cli, nil
}

func dialDatabase(ctx context.Context) (err error) {
	if rdbAuth, err = redis.DialCluster(ctx, cfg.Auth.Redis); err != nil {
		return
	}
	if rdbNonce, err = redis.DialCluster(ctx, cfg.Nonce.Redis); err != nil {
		return
	}
	if mdb, err = dialMongo(ctx); err != nil {
		return
	}
	return
}

func dbServe(ctx context.Context) {
	// load app configurations from database
	loadApps := func(ctx context.Context) (err error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		collection, err := getAppCollection(ctx)
		if err != nil {
			return fmt.Errorf("failed to get app collection: %w", err)
		}
		cursor, err := collection.Find(ctx, bson.D{})
		if err != nil {
			return fmt.Errorf("failed to query apps: %w", err)
		}
		var res []*xApp
		if err = cursor.All(ctx, &res); err != nil {
			return
		}
		for _, a := range res {
			if err = a.parse(); err != nil {
				return
			}
		}
		xApps = newAppIndex(res)
		return
	}
	for {
		if err := loadApps(ctx); err != nil {
			log.Warnf("failed to load apps: %v", err)
		}
		jitter := time.Duration(rand.Int63n(int64(30 * time.Second)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(45*time.Second + jitter): // [45s,75s)
		}
	}
}

// get app collection
func getAppCollection(ctx context.Context) (*mongo.Collection, error) {
	if dbAppCollection != nil {
		return dbAppCollection, nil
	}
	const collectionName = "apps"
	collection := mdb.Database(cfg.ConfigDBName).Collection(collectionName)
	if _, err := collection.Indexes().CreateOne(
		ctx,
		mongo.IndexModel{
			Keys:    bson.M{"key": 1},
			Options: options.Index().SetUnique(true),
		},
	); err != nil {
		return nil, fmt.Errorf("failed to create index: %w", err)
	}
	dbAppCollection = collection
	return collection, nil
}

// get user collection of app
func getUserCollection(
	ctx context.Context, appId string) (*mongo.Collection, error) {
	const collectionName = "users"
	if collection, ok := dbUserCollection[appId]; ok {
		return collection, nil
	}
	collection := mdb.Database(getAppDBName(appId)).Collection(collectionName)
	if _, err := collection.Indexes().CreateOne(
		ctx,
		mongo.IndexModel{
			Keys:    bson.M{"acct_ids": 1},
			Options: options.Index().SetUnique(true),
		},
	); err != nil {
		return nil, fmt.Errorf("failed to create index: %w", err)
	}
	dbUserCollection[appId] = collection
	return collection, nil
}

// get role collection of app
func getRoleCollection(
	ctx context.Context, appId string) (*mongo.Collection, error) {
	const collectionName = "roles"
	if collection, ok := dbRoleCollection[appId]; ok {
		return collection, nil
	}
	collection := mdb.Database(getAppDBName(appId)).Collection(collectionName)
	if _, err := collection.Indexes().CreateOne(
		ctx,
		mongo.IndexModel{
			Keys:    bson.M{"user_id": 1, "index": 1},
			Options: options.Index().SetUnique(true),
		},
	); err != nil {
		return nil, fmt.Errorf("failed to create index: %w", err)
	}
	dbRoleCollection[appId] = collection
	return collection, nil
}

func getRoleById(
	ctx context.Context, appId, roleId string) (_ *dbRole, err error) {
	collection, err := getRoleCollection(ctx, appId)
	if err != nil {
		return
	}
	role := &dbRole{}
	if err = collection.FindOne(
		ctx,
		bson.M{"_id": roleId},
	).Decode(role); err != nil {
		if err == mongo.ErrNoDocuments {
			err = errRoleNotFound
		} else {
			err = errDatabaseUnavailable
		}
		return
	}
	return role, nil
}

func newSess(
	ctx context.Context, app *xApp, userId string) (_ *xSess, err error) {
	token, err := newToken(app, userId)
	if err != nil {
		return
	}
	s := &xSess{
		Token:  token,
		AppId:  app.Id,
		UserId: userId,
	}
	b, _ := msgpack.Marshal(&s)
	if err = rdbAuth.Set(
		ctx, userId, util.BytesToString(b), 0).Err(); err != nil {
	}
	return s, nil
}

func checkToken(ctx context.Context, token string) (_ *xSess, err error) {
	app, userId, err := decToken(token)
	if err != nil {
		log.Warnf("failed to decode token: %v", err)
		return nil, errInvalidToken
	}
	b, err := rdbAuth.Get(ctx, userId).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil, errInvalidToken
		} else {
			log.Warnf("failed to get token from redis: %v", err)
			return nil, errDatabaseUnavailable
		}
	}
	s := &xSess{
		AppId:  app.Id,
		UserId: userId,
		app:    app,
	}
	if err = msgpack.Unmarshal(b, &s); err != nil {
		log.Warnf("failed to decode SessData: %v", err)
		return nil, errMalformedSessData
	}
	if s.Token != token {
		return nil, errInvalidToken
	}
	return s, nil
}

func checkNonce(ctx context.Context, appId, nonce string) (ok bool, err error) {
	key := fmt.Sprintf("%s$%s", appId, nonce)
	return rdbNonce.SetNX(ctx, key, "", cfg.Nonce.timeout).Result()
}

func loginUser(
	ctx context.Context, app *xApp, userIp string, acctIds []string) (
	_ *dbUser, _ *xSess, err error) {
	if len(acctIds) > dbMaxAcctPerUser {
		err = newInvalidArgumentError("too many acct ids")
		return
	}
	collection, err := getUserCollection(ctx, app.Id)
	if err != nil {
		return
	}
	now := time.Now()
	user := &dbUser{
		Id:         newUserId(app.Key),
		CreateTime: now,
		CreateIp:   userIp,
	}
	// 这里正确执行隐含了一个前置条件，acct_ids字段必须是索引。
	// 当给进来的acct_ids列表可以映射到多个User的时候addToSet必然会失败，
	// 从而可以保证参数 acct *---1 User 的映射关系成立。
	if err = collection.FindOneAndUpdate(
		ctx,
		bson.M{"acct_ids": bson.M{"$elemMatch": bson.M{"$in": acctIds}}},
		bson.M{
			"$set":         bson.M{"login_time": now, "login_ip": userIp},
			"$addToSet":    bson.M{"acct_ids": bson.M{"$each": acctIds}},
			"$setOnInsert": user,
		},
		options.FindOneAndUpdate().SetUpsert(true),
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(user); err != nil {
		log.Warnf("failed to access mongo: %v", err)
		err = errDatabaseUnavailable
		return
	}

	limitUserAcctCount(ctx, collection, user)

	// 生成会话
	sess, err := newSess(ctx, app, user.Id)
	if err != nil {
		return
	}

	return user, sess, nil
}

func bindAcctToUser(
	ctx context.Context, appId, userId string, acctIds []string) (err error) {
	if len(acctIds) > dbMaxAcctPerUser {
		err = newInvalidArgumentError("too many acct ids")
		return
	}
	collection, err := getUserCollection(ctx, appId)
	if err != nil {
		return
	}
	user := &dbUser{}
	if err = collection.FindOneAndUpdate(
		ctx,
		bson.M{"_id": userId},
		bson.M{"$addToSet": bson.M{"acct_ids": bson.M{"$each": acctIds}}},
		options.FindOneAndUpdate().SetUpsert(false),
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(user); err != nil {
		if err == mongo.ErrNoDocuments {
			return errUserNotFound
		} else {
			log.Warnf("failed to access mongo: %v", err)
			return errDatabaseUnavailable
		}
	}

	limitUserAcctCount(ctx, collection, user)

	return
}

// 成不成功无所谓，尽可能保证即可
func limitUserAcctCount(
	ctx context.Context, collection *mongo.Collection, user *dbUser) {
	if len(user.AcctIds) > dbMaxAcctPerUser {
		if _, err := collection.UpdateOne(
			ctx,
			bson.M{"_id": user.Id},
			bson.M{
				"$push": bson.M{
					"acct_ids": bson.M{
						"$each":  []string{},
						"$slice": -dbMaxAcctPerUser,
					},
				},
			},
		); err != nil {
			log.Warnf("failed to slice acct ids: %v, %v, %v",
				user.Id, len(user.AcctIds), err)
		}
	}
}

func setUserMetadata(
	ctx context.Context, appId, userId string,
	md map[string]string) (err error) {
	collection, err := getUserCollection(ctx, appId)
	if err != nil {
		return
	}
	set, unset := bson.M{}, bson.M{}
	for key, val := range md {
		if val != "" {
			set["metadata."+key] = val
		} else {
			unset["metadata."+key] = 1
		}
	}
	if _, err = collection.UpdateOne(
		ctx,
		bson.M{"_id": userId},
		bson.M{"$set": set, "$unset": unset},
	); err != nil {
		log.Warnf("failed to access user: %v", err)
		return errDatabaseUnavailable
	}
	return
}

func listRoles(
	ctx context.Context, appId, userId string) (_ []*dbRole, err error) {
	collection, err := getRoleCollection(ctx, appId)
	if err != nil {
		return
	}
	cur, err := collection.Find(ctx, bson.M{"user_id": userId})
	if err != nil {
		return
	}
	var roles []*dbRole
	if err = cur.All(ctx, &roles); err != nil {
		return
	}
	return roles, nil
}

func createRole(
	ctx context.Context, appId, userId string, index uint32) (
	_ *dbRole, err error) {
	app := xApps.findById(appId)
	if app == nil {
		return nil, errInvalidAppId
	}
	collection, err := getRoleCollection(ctx, appId)
	if err != nil {
		return
	}
	role := &dbRole{
		Id:         newRoleId(app.Key),
		UserId:     userId,
		Index:      index,
		CreateTime: time.Now(),
	}
	if _, err = collection.InsertOne(ctx, role); err != nil {
		return
	}
	return role, nil
}

func signInRole(
	ctx context.Context, appId, userId, roleId string) (err error) {
	collection, err := getRoleCollection(ctx, appId)
	if err != nil {
		return
	}
	var role dbRole
	if err = collection.FindOneAndUpdate(
		ctx,
		bson.M{"_id": roleId, "user_id": userId},
		bson.M{"$set": bson.M{"sign_in_time": time.Now()}},
	).Decode(&role); err != nil {
		if err == mongo.ErrNoDocuments {
			err = errRoleNotFound
		} else {
			err = errDatabaseUnavailable
		}
		return
	}
	// update sess data
	b, _ := msgpack.Marshal(&xSessData{
		RoleId:    roleId,
		RoleIndex: role.Index,
	})
	if err = luaUpdateSessData.Run(
		ctx, rdbAuth, []string{userId}, b).Err(); err != nil {
		if err == redis.Nil {
			return errInvalidToken
		} else {
			log.Warnf("failed to update session: %v", err)
			return errDatabaseUnavailable
		}
	}
	return
}

func setRoleMetadata(
	ctx context.Context, appId, userId, roleId string,
	md map[string]string) (err error) {
	collection, err := getRoleCollection(ctx, appId)
	if err != nil {
		return
	}
	set, unset := bson.M{}, bson.M{}
	for key, val := range md {
		if val != "" {
			set["metadata."+key] = val
		} else {
			unset["metadata."+key] = 1
		}
	}
	if _, err = collection.UpdateOne(
		ctx,
		bson.M{"_id": roleId, "user_id": userId},
		bson.M{"$set": set, "$unset": unset},
	); err != nil {
		return
	}
	return
}
