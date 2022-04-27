package utils

import (
	"fmt"
	bson2 "go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
	"math"
	"strconv"
	"strings"

	"github.com/vinllen/mgo"
	"github.com/vinllen/mgo/bson"

	"sort"
)

var (
	QueryTs = "ts"
	localDB = "local"
)

const (
	DBRefRef = "$ref"
	DBRefId  = "$id"
	DBRefDb  = "$db"

	CollectionCapped           = "CollectionScan died due to position in capped" // bigger than 3.0
	CollectionCappedLowVersion = "UnknownError"                                  // <= 3.0 version
)

// for UT only
var (
	GetAllTimestampInUTInput map[string]Pair // replicaSet/MongoS name => <oldest timestamp, newest timestamp>
)

/************************************************/

type MongoSource struct {
	URL         string
	ReplicaName string
	Gids        []string
}

func (ms *MongoSource) String() string {
	return fmt.Sprintf("url[%v], name[%v]", BlockMongoUrlPassword(ms.URL, "***"), ms.ReplicaName)
}

// get db version, return string with format like "3.0.1"
func GetDBVersion(conn *MongoCommunityConn) (string, error) {

	res, err := conn.Client.Database("admin").
		RunCommand(conn.ctx, bson2.D{{"buildInfo", 1}}).DecodeBytes()
	if err != nil {
		return "", err
	}

	ver, ok := res.Lookup("version").StringValueOK()
	if !ok {
		return "", fmt.Errorf("buildInfo do not have version")
	}

	return ver, nil
}

// get current db version and compare to threshold. Return whether the result
// is bigger or equal to the input threshold.
func GetAndCompareVersion(conn *MongoCommunityConn, threshold string, compare string) (bool, error) {
	var err error
	if compare == "" {
		if conn == nil {
			return false, nil
		}

		compare, err = GetDBVersion(conn)
		if err != nil {
			return false, err
		}
	}

	compareArr := strings.Split(compare, ".")
	thresholdArr := strings.Split(threshold, ".")
	if len(compareArr) < 2 || len(thresholdArr) < 2 {
		return false, nil
	}

	for i := 0; i < 2; i++ {
		compareEle, errC := strconv.Atoi(compareArr[i])
		thresholdEle, errT := strconv.Atoi(thresholdArr[i])
		if errC != nil || errT != nil {
			return false, fmt.Errorf("errC:[%v], errT:[%v]", errC, errT)
		}

		if compareEle > thresholdEle {
			return true, nil
		} else if compareEle < thresholdEle {
			return false, fmt.Errorf("compare[%v] < threshold[%v]", compare, threshold)
		}
	}
	return true, nil
}

func IsNotFound(err error) bool {
	return err.Error() == mgo.ErrNotFound.Error()
}

func ApplyOpsFilter(key string) bool {
	// convert to map if has more later
	k := strings.TrimSpace(key)
	if k == "$db" {
		// 40621, $db is not allowed in OP_QUERY requests
		return true
	} else if k == "ui" {
		return true
	}

	return false
}

func getOplogTimestamp(conn *MongoCommunityConn, sortType int) (primitive.DateTime, error) {
	var result bson2.M
	opts := options.FindOne().SetSort(bson2.D{{"$natural", sortType}})
	err := conn.Client.Database(localDB).Collection(OplogNS).FindOne(nil, bson2.M{}, opts).Decode(&result)
	if err != nil {
		return 0, err
	}

	return primitive.DateTime(int64(result["ts"].(primitive.Timestamp).T)<<32 +
		int64(result["ts"].(primitive.Timestamp).I)), nil
}

// get newest oplog
func GetNewestTimestampByConn(conn *MongoCommunityConn) (primitive.DateTime, error) {

	return getOplogTimestamp(conn, -1)
}

// get oldest oplog
func GetOldestTimestampByConn(conn *MongoCommunityConn) (primitive.DateTime, error) {

	return getOplogTimestamp(conn, 1)
}

func GetNewestTimestampBySession(session *mgo.Session) (primitive.DateTime, error) {
	var retMap map[string]interface{}
	err := session.DB(localDB).C(OplogNS).Find(bson.M{}).Sort("-$natural").Limit(1).One(&retMap)
	if err != nil {
		return 0, err
	}
	return retMap[QueryTs].(primitive.DateTime), nil
}

func GetOldestTimestampBySession(session *mgo.Session) (primitive.DateTime, error) {
	var retMap map[string]interface{}
	err := session.DB(localDB).C(OplogNS).Find(bson.M{}).Limit(1).One(&retMap)
	if err != nil {
		return 0, err
	}
	return retMap[QueryTs].(primitive.DateTime), nil
}

func GetNewestTimestampByUrl(url string, fromMongoS bool, sslRootFile string) (primitive.DateTime, error) {
	var conn *MongoCommunityConn
	var err error
	if conn, err = NewMongoCommunityConn(url, VarMongoConnectModeSecondaryPreferred, true,
		ReadWriteConcernDefault, ReadWriteConcernDefault, sslRootFile); conn == nil || err != nil {
		return 0, err
	}
	defer conn.Close()

	if fromMongoS {
		date := primitive.DateTime(conn.CurrentDate())
		return date, nil
	}

	return GetNewestTimestampByConn(conn)
}

func GetOldestTimestampByUrl(url string, fromMongoS bool, sslRootFile string) (primitive.DateTime, error) {
	if fromMongoS {
		return 0, nil
	}

	var conn *MongoCommunityConn
	var err error
	if conn, err = NewMongoCommunityConn(url, VarMongoConnectModeSecondaryPreferred, true,
		ReadWriteConcernDefault, ReadWriteConcernDefault, sslRootFile); conn == nil || err != nil {
		return 0, err
	}
	defer conn.Close()

	return GetOldestTimestampByConn(conn)
}

// deprecated
func IsFromMongos(url string) (bool, error) {
	var conn *MongoConn
	var err error
	if conn, err = NewMongoConn(url, VarMongoConnectModeSecondaryPreferred, true,
		ReadWriteConcernDefault, ReadWriteConcernDefault, ""); conn == nil || err != nil {
		return false, err
	}
	return conn.IsMongos(), nil
}

// record the oldest and newest timestamp of each mongod
type TimestampNode struct {
	Oldest primitive.DateTime
	Newest primitive.DateTime
}

/*
 * get all newest timestamp
 * return:
 *     map: whole timestamp map, key: replset name, value: struct that includes the newest and oldest timestamp
 *     primitive.DateTime: the biggest of the newest timestamp
 *     primitive.DateTime: the smallest of the newest timestamp
 *     error: error
 */
func GetAllTimestamp(sources []*MongoSource, sslRootFile string) (map[string]TimestampNode, primitive.DateTime,
	primitive.DateTime, primitive.DateTime, primitive.DateTime, error) {
	smallestNew := primitive.DateTime(math.MaxInt64)
	biggestNew := primitive.DateTime(0)
	smallestOld := primitive.DateTime(math.MaxInt64)
	biggestOld := primitive.DateTime(0)
	tsMap := make(map[string]TimestampNode)

	for _, src := range sources {
		newest, err := GetNewestTimestampByUrl(src.URL, false, sslRootFile)
		if err != nil {
			return nil, 0, 0, 0, 0, err
		} else if newest == 0 {
			return nil, 0, 0, 0, 0, fmt.Errorf("illegal newest timestamp == 0")
		}

		oldest, err := GetOldestTimestampByUrl(src.URL, false, sslRootFile)
		if err != nil {
			return nil, 0, 0, 0, 0, err
		}
		tsMap[src.ReplicaName] = TimestampNode{
			Oldest: oldest,
			Newest: newest,
		}

		if newest > biggestNew {
			biggestNew = newest
		}
		if newest < smallestNew {
			smallestNew = newest
		}
		if oldest > biggestOld {
			biggestOld = oldest
		}
		if oldest < smallestOld {
			smallestOld = oldest
		}
	}
	return tsMap, biggestNew, smallestNew, biggestOld, smallestOld, nil
}

// only used in unit test
func GetAllTimestampInUT() (map[string]TimestampNode, primitive.DateTime,
	primitive.DateTime, primitive.DateTime, primitive.DateTime, error) {
	smallestNew := primitive.DateTime(math.MaxInt64)
	biggestNew := primitive.DateTime(0)
	smallestOld := primitive.DateTime(math.MaxInt64)
	biggestOld := primitive.DateTime(0)
	tsMap := make(map[string]TimestampNode)
	for name, ele := range GetAllTimestampInUTInput {
		oldest := ele.First.(primitive.DateTime)
		newest := ele.Second.(primitive.DateTime)
		tsMap[name] = TimestampNode{
			Oldest: oldest,
			Newest: newest,
		}

		if newest > biggestNew {
			biggestNew = newest
		}
		if newest < smallestNew {
			smallestNew = newest
		}
		if oldest > biggestOld {
			biggestOld = oldest
		}
		if oldest < smallestOld {
			smallestOld = oldest
		}
	}

	return tsMap, biggestNew, smallestNew, biggestOld, smallestOld, nil
}

func IsCollectionCappedError(err error) bool {
	errMsg := err.Error()
	if strings.Contains(errMsg, CollectionCapped) || strings.Contains(errMsg, CollectionCappedLowVersion) {
		return true
	}
	return false
}

// used to handle bulk return error
func FindFirstErrorIndexAndMessage(error string) (int, string, bool) {
	subIndex := "index["
	subMsg := "msg["
	subDup := "dup["
	index := strings.Index(error, subIndex)
	if index == -1 {
		return index, "", false
	}

	indexVal := 0
	for i := index + len(subIndex); i < len(error) && error[i] != ']'; i++ {
		// fmt.Printf("%c %d\n", rune(error[i]), int(error[i] - '0'))
		indexVal = indexVal*10 + int(error[i]-'0')
	}

	index = strings.Index(error, subMsg)
	if index == -1 {
		return indexVal, "", false
	}

	i := index + len(subMsg)
	stack := 0
	for ; i < len(error); i++ {
		if error[i] == ']' {
			if stack == 0 {
				break
			} else {
				stack -= 1
			}
		} else if error[i] == '[' {
			stack += 1
		}
	}
	msg := error[index+len(subMsg) : i]

	index = strings.Index(error, subDup)
	if index == -1 {
		return indexVal, msg, false
	}
	i = index + len(subMsg)
	for ; i < len(error) && error[i] != ']'; i++ {
	}
	dupVal := error[index+len(subMsg) : i]

	return indexVal, msg, dupVal == "true"
}

func HasUniqueIndex(index []mgo.Index) bool {
	for _, idx := range index {
		if !strings.HasPrefix(idx.Name, "_id") && idx.Unique == true {
			return true
		}
	}
	return false
}

/**
 * return db namespace. return:
 *     @[]NS: namespace list, e.g., []{"a.b", "a.c"}
 *     @map[string][]string: db->collection map. e.g., "a"->[]string{"b", "c"}
 *     @error: error info
 */
func GetDbNamespace(url string, filterFunc func(name string) bool, sslRootFile string) ([]NS, map[string][]string, error) {
	var err error
	var conn *MongoCommunityConn
	if conn, err = NewMongoCommunityConn(url, VarMongoConnectModePrimary, true,
		ReadWriteConcernLocal, ReadWriteConcernDefault, sslRootFile); conn == nil || err != nil {
		return nil, nil, err
	}
	defer conn.Close()

	var dbNames []string
	if dbNames, err = conn.Client.ListDatabaseNames(nil, bson2.M{}); err != nil {
		err = fmt.Errorf("get database names of mongodb[%s] error: %v", url, err)
		return nil, nil, err
	}
	// sort by db names
	sort.Strings(dbNames)

	nsList := make([]NS, 0, 128)
	for _, db := range dbNames {
		colNames, err := conn.Client.Database(db).ListCollectionNames(nil, bson2.M{})
		if err != nil {
			err = fmt.Errorf("get collection names of mongodb[%s] db[%v] error: %v", url, db, err)
			return nil, nil, err
		}

		// LOG.Info("db[%v] colNames: %v", db, colNames)
		for _, col := range colNames {
			ns := NS{Database: db, Collection: col}
			if strings.HasPrefix(col, "system.") {
				continue
			}
			if filterFunc != nil && filterFunc(ns.Str()) {
				// LOG.Debug("Namespace is filtered. %v", ns.Str())
				continue
			}
			nsList = append(nsList, ns)
		}
	}

	// copy, convert nsList to map
	nsMap := make(map[string][]string, 0)
	for _, ns := range nsList {
		if _, ok := nsMap[ns.Database]; !ok {
			nsMap[ns.Database] = make([]string, 0)
		}
		nsMap[ns.Database] = append(nsMap[ns.Database], ns.Collection)
	}

	return nsList, nsMap, nil
}

/**
 * return all namespace. return:
 *     @map[NS]struct{}: namespace set where key is the namespace while value is useless, e.g., "a.b"->nil, "a.c"->nil
 *     @map[string][]string: db->collection map. e.g., "a"->[]string{"b", "c"}
 *     @error: error info
 */
func GetAllNamespace(sources []*MongoSource, filterFunc func(name string) bool,
	sslRootFile string) (map[NS]struct{}, map[string][]string, error) {
	nsSet := make(map[NS]struct{})
	for _, src := range sources {
		nsList, _, err := GetDbNamespace(src.URL, filterFunc, sslRootFile)
		if err != nil {
			return nil, nil, err
		}
		for _, ns := range nsList {
			nsSet[ns] = struct{}{}
		}
	}

	// copy
	nsMap := make(map[string][]string, len(sources))
	for ns := range nsSet {
		if _, ok := nsMap[ns.Database]; !ok {
			nsMap[ns.Database] = make([]string, 0)
		}
		nsMap[ns.Database] = append(nsMap[ns.Database], ns.Collection)
	}

	return nsSet, nsMap, nil
}
