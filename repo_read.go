package main

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/jmoiron/sqlx/types"
	"golang.org/x/xerrors"
)

type FetchedDBEntry struct {
	ID           int64          `db:"id"`
	ParentID     int64          `db:"parent_id"`
	EntryUUID    string         `db:"uuid"`
	Created      time.Time      `db:"created"`
	Updated      time.Time      `db:"updated"`
	RDNOrig      string         `db:"rdn_orig"`
	AttrsOrig    types.JSONText `db:"attrs_orig"`
	Member       types.JSONText `db:"member"`    // No real column in the table
	MemberOf     types.JSONText `db:"member_of"` // No real column in the table
	DNOrig       string         `db:"dn_orig"`   // No real clumn in t he table
	Count        int32          `db:"count"`     // No real column in the table
	ParentDNOrig string         // No real column in t he table
}

type FetchedMember struct {
	RDNOrig      string `db:"r`
	ParentID     int64  `db:"p`
	AttrNameNorm string `db:"a`
}

func (e *FetchedDBEntry) IsDC() bool {
	return e.ParentID == ROOT_ID
}

func (e *FetchedDBEntry) Members(dnOrigCache map[int64]string, suffix string) (map[string][]string, error) {
	if len(e.Member) > 0 {
		jsonMap := make(map[string][]string)
		jsonArray := []map[string]string{}
		err := e.Member.Unmarshal(&jsonArray)
		if err != nil {
			return nil, err
		}
		for _, m := range jsonArray {
			v, ok := jsonMap[m["a"]]
			if !ok {
				v = []string{}
			}
			pid, err := strconv.ParseInt(m["p"], 10, 64)
			if err != nil {
				return nil, xerrors.Errorf("Invalid parent_id: %s", m["p"])
			}
			parentDNOrig, ok := dnOrigCache[pid]
			if !ok {
				return nil, xerrors.Errorf("No cached: %s", m["p"])
			}

			s := parentDNOrig
			if suffix != "" {
				if parentDNOrig == "" {
					s = suffix
				} else {
					s += "," + suffix
				}
			}

			v = append(v, m["r"]+","+s)

			jsonMap[m["a"]] = v
		}
		return jsonMap, nil
	}
	return nil, nil
}

func (e *FetchedDBEntry) MemberOfs(dnOrigCache map[int64]string, suffix string) ([]string, error) {
	if len(e.MemberOf) > 0 {
		jsonArray := []map[string]string{}
		err := e.MemberOf.Unmarshal(&jsonArray)
		if err != nil {
			return nil, err
		}

		dns := make([]string, len(jsonArray))

		for i, m := range jsonArray {
			pid, err := strconv.ParseInt(m["p"], 10, 64)
			if err != nil {
				return nil, xerrors.Errorf("Invalid parent_id: %s", m["p"])
			}
			parentDNOrig, ok := dnOrigCache[pid]
			if !ok {
				return nil, xerrors.Errorf("No cached: %s", m["p"])
			}

			var s string
			if parentDNOrig == "" {
				s = suffix
			} else {
				s = parentDNOrig + "," + suffix
			}

			dns[i] = m["r"] + "," + s
		}
		return dns, nil
	}
	return nil, nil
}

func (e *FetchedDBEntry) GetAttrsOrig() map[string][]string {
	if len(e.AttrsOrig) > 0 {
		jsonMap := make(map[string][]string)
		e.AttrsOrig.Unmarshal(&jsonMap)

		if len(e.MemberOf) > 0 {
			jsonArray := []string{}
			e.MemberOf.Unmarshal(&jsonArray)
			jsonMap["memberOf"] = jsonArray
		}

		return jsonMap
	}
	return nil
}

func (e *FetchedDBEntry) Clear() {
	e.ID = 0
	e.DNOrig = ""
	e.AttrsOrig = nil
	e.MemberOf = nil
	e.Count = 0
}

type FetchedDNOrig struct {
	ID     int64  `db:"id"`
	DNOrig string `db:"dn_orig"`
}

func (r *Repository) Search(baseDN *DN, scope int, q *Query, reqMemberAttrs []string, reqMemberOf bool, handler func(entry *SearchEntry) error) (int32, int32, error) {
	// TODO
	dnOrigCache := q.dnOrigCache

	baseDNID, cid, err := r.collectParentIDs(baseDN, scope, dnOrigCache)
	if err != nil {
		return 0, 0, err
	}

	where, err := appenScopeFilter(scope, q, baseDNID, cid)
	if err != nil {
		return 0, 0, err
	}

	var memberCol string
	var memberJoin string
	if len(reqMemberAttrs) > 0 {
		// TODO bind parameter
		in := make([]string, len(reqMemberAttrs))
		for i, v := range reqMemberAttrs {
			in[i] = "'" + v + "'"
		}

		memberCol = ", to_jsonb(ma.member_array) as member"
		memberJoin = fmt.Sprintf(`, LATERAL (
			SELECT ARRAY (
				SELECT jsonb_build_object('r', ee.rdn_orig, 'p', ee.parent_id::::TEXT, 'a', lm.attr_name_norm) 
					FROM ldap_member lm
						JOIN ldap_entry ee ON ee.id = lm.member_of_id
					WHERE lm.member_id = e.id AND lm.attr_name_norm IN (%s)
			) AS member_array
		) ma`, strings.Join(in, ", "))
	}

	var memberOfCol string
	var memberOfJoin string
	if reqMemberOf {
		memberOfCol = ", to_jsonb(moa.member_of_array) as member_of"
		memberOfJoin = `, LATERAL (
			SELECT ARRAY (
				SELECT jsonb_build_object('r', ee.rdn_orig, 'p', ee.parent_id::::TEXT) 
					FROM ldap_member lm
						JOIN ldap_entry ee ON ee.id = lm.member_id
					WHERE lm.member_of_id = e.id
			) AS member_of_array
		) moa`
	}

	// LEFT JOIN LATERAL(
	// 		SELECT t.rdn_norm, t.rdn_orig FROM ldap_tree t WHERE t.id = e.parent_id
	// 	) p ON true
	searchQuery := fmt.Sprintf(`SELECT e.id, e.parent_id, e.uuid, e.created, e.updated, e.rdn_orig, '' AS dn_orig, e.attrs_orig %s %s, count(e.id) over() AS count
		FROM ldap_entry e %s %s
		WHERE %s
		LIMIT :pageSize OFFSET :offset`, memberCol, memberOfCol, memberJoin, memberOfJoin, where)

	log.Printf("Fetch Query: %s Params: %v", searchQuery, q.Params)

	var fetchStmt *sqlx.NamedStmt
	var ok bool
	if fetchStmt, ok = filterStmtMap.Get(searchQuery); !ok {
		// cache
		fetchStmt, err = r.db.PrepareNamed(searchQuery)
		if err != nil {
			return 0, 0, err
		}
		filterStmtMap.Put(searchQuery, fetchStmt)
	}

	var rows *sqlx.Rows
	rows, err = fetchStmt.Queryx(q.Params)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()

	dbEntry := FetchedDBEntry{}
	var maxCount int32 = 0
	var count int32 = 0

	for rows.Next() {
		err := rows.StructScan(&dbEntry)
		if err != nil {
			log.Printf("error: DBEntry struct mapping error: %#v", err)
			return 0, 0, err
		}

		var dnOrig string
		if dnOrig, ok = dnOrigCache[dbEntry.ID]; !ok {
			parentDNOrig, ok := dnOrigCache[dbEntry.ParentID]
			if !ok {
				log.Printf("warn: Failed to retrieve parent by parent_id: %d. The parent might be removed or renamed.", dbEntry.ParentID)
				// TODO return busy?
				return 0, 0, xerrors.Errorf("Failed to retrieve parent by parent_id: %d", dbEntry.ParentID)
			}

			// Set dn_orig using cache from fetching ldap_tree table
			if parentDNOrig != "" {
				dnOrig = fmt.Sprintf("%s,%s", dbEntry.RDNOrig, parentDNOrig)
			} else {
				dnOrig = dbEntry.RDNOrig
			}
		}
		dbEntry.DNOrig = dnOrig

		readEntry, err := mapper.FetchedDBEntryToSearchEntry(&dbEntry, dnOrigCache)
		if err != nil {
			log.Printf("error: Mapper error: %#v", err)
			return 0, 0, err
		}

		if maxCount == 0 {
			maxCount = dbEntry.Count
		}

		err = handler(readEntry)
		if err != nil {
			log.Printf("error: Handler error: %#v", err)
			return 0, 0, err
		}

		count++
		dbEntry.Clear()
	}

	err = rows.Err()
	if err != nil {
		log.Printf("error: Search error: %#v", err)
		return 0, 0, err
	}

	return maxCount, count, nil
}

func getDC(tx *sqlx.Tx) (*FetchedDBEntry, error) {
	var err error
	dest := FetchedDBEntry{}

	if tx != nil {
		err = tx.NamedStmt(getDCStmt).Get(&dest, map[string]interface{}{})
	} else {
		err = getDCStmt.Get(&dest, map[string]interface{}{})
	}
	if err != nil {
		return nil, err
	}

	return &dest, nil
}

func getDCDNOrig(tx *sqlx.Tx) (*FetchedDNOrig, error) {
	var err error
	dest := FetchedDNOrig{}

	if tx != nil {
		err = tx.NamedStmt(getDCDNOrigStmt).Get(&dest, map[string]interface{}{})
	} else {
		err = getDCDNOrigStmt.Get(&dest, map[string]interface{}{})
	}
	if err != nil {
		if err == sql.ErrNoRows {
			log.Printf("warn: Not found DC in the tree.")
			return nil, NewNoSuchObject()
		}
		return nil, xerrors.Errorf("Failed to get DC DN. err: %w", err)
	}

	return &dest, nil
}

type FindOption struct {
	Lock       bool
	FetchAttrs bool
	FetchCred  bool
}

func (r *Repository) FindByDN(tx *sqlx.Tx, dn *DN, option *FindOption) (*FetchedDBEntry, error) {
	if dn == nil {
		return nil, xerrors.Errorf("Failed to find by DN because the DN is nil. You might try to find parent of DC entry.")
	}
	if dn.IsDC() {
		return getDC(tx)
	}

	stmt, params, err := r.PrepareFindByDN(dn, option)
	if err != nil {
		return nil, err
	}

	dest := FetchedDBEntry{}

	if tx != nil {
		err = tx.NamedStmt(stmt).Get(&dest, params)
	} else {
		err = stmt.Get(&dest, params)
	}
	if err != nil {
		return nil, err
	}

	return &dest, nil
}

func (r *Repository) PrepareFindByDN(dn *DN, option *FindOption) (*sqlx.NamedStmt, map[string]interface{}, error) {
	//  Key for stmt cache
	key := fmt.Sprintf("LOCK:%v/DEPTH:%d", option.Lock, len(dn.dnNorm))

	if stmt, ok := findByDNStmtCache.Get(key); ok {
		// Already cached, make params only
		depthj := len(dn.dnNorm)
		last := depthj - 1
		params := make(map[string]interface{}, depthj)

		for i := last; i >= 0; i-- {
			params[fmt.Sprintf("rdn_norm_%d", i)] = dn.dnNorm[i]
		}
		return stmt, params, nil
	}

	// Not cached yet, create query and params, then cache the stmt
	q, params := r.CreateFindByDNQuery(dn, option)

	stmt, err := r.db.PrepareNamed(q)
	if err != nil {
		return nil, nil, err
	}
	findByDNStmtCache.Put(key, stmt)

	return stmt, params, nil
}

func (r *Repository) PrepareFindTreeByDN(dn *DN) (*sqlx.NamedStmt, map[string]interface{}, error) {
	//  Key for stmt cache
	key := fmt.Sprintf("TREE/DEPTH:%d", len(dn.dnNorm))

	if stmt, ok := findByDNStmtCache.Get(key); ok {
		// Already cached, make params only
		depthj := len(dn.dnNorm)
		last := depthj - 1
		params := make(map[string]interface{}, depthj)

		for i := last; i >= 0; i-- {
			params[fmt.Sprintf("rdn_norm_%d", i)] = dn.dnNorm[i]
		}
		return stmt, params, nil
	}

	// Not cached yet, create query and params, then cache the stmt
	dnq, params := r.CreateFindByDNQuery(dn, &FindOption{Lock: false})

	q := fmt.Sprintf(`WITH RECURSIVE dn AS
	(
		%s
	),
	child (depth, dn_orig, id, parent_id) AS
	(
		SELECT 0, dn.dn_orig::::TEXT AS dn_orig, e.id, e.parent_id
			FROM ldap_tree e, dn WHERE e.id = dn.id 
			UNION ALL
				SELECT
					child.depth + 1,
					CASE child.dn_orig
						WHEN '' THEN e.rdn_orig 
						ELSE e.rdn_orig || ',' || child.dn_orig
					END,
					e.id,
					e.parent_id
				FROM ldap_tree e, child
				WHERE e.parent_id = child.id
	)
	SELECT id, dn_orig FROM child ORDER BY depth`, dnq)

	log.Printf("PrepareFindTreeByDN: %s", q)

	stmt, err := r.db.PrepareNamed(q)
	if err != nil {
		return nil, nil, err
	}
	findByDNStmtCache.Put(key, stmt)

	return stmt, params, nil
}

func (r *Repository) CreateFindByDNQuery(dn *DN, option *FindOption) (string, map[string]interface{}) {
	// 	SELECT e.id, e.rdn_orig || ',' || e0.rdn_orig || ',' || e1.rdn_orig || ',' || e2.rdn_orig AS dn_orig
	//	   FROM ldap_tree dc
	//     INNER JOIN ldap_tree e2 ON e2.parent_id = dc.id
	//     INNER JOIN ldap_tree e1 ON e1.parent_id = e2.id
	//     INNER JOIN ldap_tree e0 ON e0.parent_id = e1.id
	//     INNER JOIN ldap_entry e ON e.parent_id = e0.id
	//     WHERE dc.parent_id = 0 AND e2.rdn_norm = 'ou=mycompany' AND e1.rdn_norm = 'ou=mysection' AND e0.rdn_norm = 'ou=mydept' AND e.rdn_norm = 'cn=mygroup'
	//     FOR UPDATE ldap_entry

	var fetchAttrsProjection string
	var memberJoin string
	if option.FetchAttrs {
		fetchAttrsProjection += `, e0.parent_id, e0.rdn_orig, e0.attrs_orig, to_jsonb(ma.member_array) as member`
		// TODO use join when the entry's schema has member
		memberJoin += `, LATERAL (
				SELECT ARRAY (
					SELECT jsonb_build_object('r', ee.rdn_orig, 'p', ee.parent_id::::TEXT, 'a', lm.attr_name_norm) 
						FROM ldap_member lm
							JOIN ldap_entry ee ON ee.id = lm.member_of_id
						WHERE lm.member_id = e0.id 
				) AS member_array
			) ma`
	}
	if option.FetchCred {
		fetchAttrsProjection += `, e0.attrs_norm->>'userPassword' as cred`
	}

	if dn.IsDC() {
		q := fmt.Sprintf(`SELECT id, '' as dn_orig %s FROM ldap_entry
		WHERE parent_id = %d`, fetchAttrsProjection, ROOT_ID)
		return q, map[string]interface{}{}
	}

	size := len(dn.dnNorm)
	last := size - 1
	params := make(map[string]interface{}, size)

	projection := make([]string, size)
	join := make([]string, size)
	where := make([]string, size)

	for i := last; i >= 0; i-- {
		projection[i] = fmt.Sprintf("e%d.rdn_orig", i)
		if i == last {
			join[last-i] = fmt.Sprintf("INNER JOIN ldap_tree e%d ON e%d.parent_id = dc.id", i, i)
		} else if i > 0 {
			join[last-i] = fmt.Sprintf("INNER JOIN ldap_tree e%d ON e%d.parent_id = e%d.id", i, i, i+1)
		} else {
			join[last-i] = "INNER JOIN ldap_entry e0 ON e0.parent_id = e1.id"
		}
		where[last-i] = fmt.Sprintf("e%d.rdn_norm = :rdn_norm_%d", i, i)

		params[fmt.Sprintf("rdn_norm_%d", i)] = dn.dnNorm[i]
	}

	q := fmt.Sprintf(`SELECT e0.id, %s AS dn_orig %s
		FROM ldap_tree dc %s %s
		WHERE dc.parent_id = %d AND %s`,
		strings.Join(projection, " || ',' || "), fetchAttrsProjection,
		strings.Join(join, " "), memberJoin,
		ROOT_ID, strings.Join(where, " AND "))

	if option.Lock {
		q += " FOR UPDATE"
	}

	log.Printf("debug: findByDN query: %s, params: %v", q, params)

	return q, params
}

func collectAllNodeNorm() (map[string]int64, error) {
	dc, err := getDCDNOrig(nil)
	if err != nil {
		return nil, err
	}
	nodes, err := collectNodeNormByParentID(dc.ID)
	if err != nil {
		return nil, err
	}

	cache := make(map[string]int64, len(nodes)+1)
	cache[""] = dc.ID

	for _, n := range nodes {
		cache[n.DNNorm] = n.ID
	}

	return cache, nil
}

func collectAllNodeOrig(tx *sqlx.Tx) (map[int64]string, error) {
	dc, err := getDCDNOrig(tx)
	if err != nil {
		return nil, err
	}
	nodes, err := collectNodeOrigByParentID(tx, dc.ID)
	if err != nil {
		return nil, err
	}

	cache := make(map[int64]string, len(nodes)+1)
	cache[dc.ID] = dc.DNOrig

	for _, n := range nodes {
		cache[n.ID] = n.DNOrig
	}

	return cache, nil
}

func collectNodeOrigByParentID(tx *sqlx.Tx, parentID int64) ([]*FetchedDNOrig, error) {
	if parentID == ROOT_ID {
		return nil, xerrors.Errorf("Invalid parentID: %d", parentID)
	}
	var rows *sqlx.Rows
	var err error
	if tx != nil {
		rows, err = tx.NamedStmt(collectNodeOrigByParentIDStmt).Queryx(map[string]interface{}{
			"parent_id": parentID,
		})
	} else {
		rows, err = collectNodeOrigByParentIDStmt.Queryx(map[string]interface{}{
			"parent_id": parentID,
		})
	}
	if err != nil {
		return nil, xerrors.Errorf("Failed to fetch child ID by parentID: %s, err: %w", parentID, err)
	}
	defer rows.Close()

	list := []*FetchedDNOrig{}
	for rows.Next() {
		child := FetchedDNOrig{}
		rows.StructScan(&child)
		list = append(list, &child)
	}

	err = rows.Err()
	if err != nil {
		log.Printf("error: Search children error: %#v", err)
		return nil, err
	}

	return list, nil
}

func (r *Repository) FindByDNWithLock(tx *sqlx.Tx, dn *DN) (*ModifyEntry, error) {
	// TODO optimize collecting all container DN orig
	dnOrigCache, err := collectAllNodeOrig(tx)
	if err != nil {
		return nil, err
	}

	entry, err := r.FindByDN(tx, dn, &FindOption{Lock: true, FetchAttrs: true})
	if err != nil {
		return nil, err
	}
	return mapper.FetchedDBEntryToModifyEntry(entry, dnOrigCache)
}

func (r *Repository) FindCredByDN(dn *DN) ([]string, error) {
	q, params := r.CreateFindByDNQuery(dn, &FindOption{Lock: false, FetchCred: true})

	key := "DEPTH:" + strconv.Itoa(len(dn.dnNorm))

	dest := struct {
		ID     int64          `db:"id"`
		DNOrig string         `db:"dn_orig"`
		Cred   types.JSONText `db:"cred"`
	}{}

	var stmt *sqlx.NamedStmt
	var ok bool

	if stmt, ok = findCredByDNStmtCache.Get(key); !ok {
		var err error
		stmt, err = r.db.PrepareNamed(q)
		if err != nil {
			return nil, xerrors.Errorf("Failed to prepare name query. query: %s, params: %v, dn: %s, err: %w", q, params, dn.DNOrigStr(), err)
		}
		findCredByDNStmtCache.Put(key, stmt)
	}

	err := stmt.Get(&dest, params)
	if err != nil {
		if isNoResult(err) {
			return nil, NewInvalidCredentials()
		}
		return nil, xerrors.Errorf("Failed to find cred by DN. dn: %s, err: %w", dn.DNOrigStr(), err)
	}

	var cred []string
	err = dest.Cred.Unmarshal(&cred)
	if err != nil {
		return nil, xerrors.Errorf("Failed to unmarshal cred array. dn: %s, err: %w", dn.DNOrigStr(), err)
	}

	return cred, nil
}

func appenScopeFilter(scope int, q *Query, baseDNID int64, childrenDNIDs []int64) (string, error) {
	// Make query based on the requested scope

	// Scope handling, one and sub need to include base.
	// 0: base
	// 1: one
	// 2: sub
	// 3: children
	var parentFilter string
	if scope == 0 {
		parentFilter = "e.id = :baseDNID"
		q.Params["baseDNID"] = baseDNID

	} else if scope == 1 {
		parentFilter = "e.parent_id = :baseDNID"
		q.Params["baseDNID"] = baseDNID

	} else if scope == 2 {
		childrenDNIDs = append(childrenDNIDs, baseDNID)
		in, params := expandIn(childrenDNIDs)
		parentFilter = "(e.id = :baseDNID OR e.parent_id IN (" + in + "))"
		q.Params["baseDNID"] = baseDNID
		for k, v := range params {
			q.Params[k] = v
		}

	} else if scope == 3 {
		childrenDNIDs = append(childrenDNIDs, baseDNID)
		in, params := expandIn(childrenDNIDs)
		parentFilter = "e.parent_id IN (" + in + ")"
		for k, v := range params {
			q.Params[k] = v
		}
	}

	var query string
	if q.Query != "" {
		query = " AND " + q.Query
	}

	return fmt.Sprintf("%s %s", parentFilter, query), nil
}

func (r *Repository) collectParentIDs(baseDN *DN, scope int, dnOrigCache map[int64]string) (int64, []int64, error) {
	// Collect parent ID(s) based on baseDN
	var baseDNID int64 = -1

	// No result case
	if !baseDN.IsContainer() && (scope == 1 || scope == 3) {
		// Need to return success response
		return 0, nil, NewSuccess()
	}

	// 0: base
	// 1: one
	// 2: sub
	// 3: children
	if scope == 0 || scope == 1 || !baseDN.IsContainer() {
		if baseDN.IsDC() {
			entry, err := getDCDNOrig(nil)
			if err != nil {
				return 0, nil, err
			}
			baseDNID = entry.ID
			dnOrigCache[entry.ID] = entry.DNOrig
			return baseDNID, []int64{}, nil
		}

		// baseDN points to entry or container.
		entry, err := r.FindByDN(nil, baseDN, &FindOption{Lock: false})
		if err != nil {
			if isNoResult(err) {
				return 0, nil, NewSuccess()
			}
			return 0, nil, err
		}
		baseDNID = entry.ID
		dnOrigCache[entry.ID] = entry.DNOrig

		return baseDNID, []int64{}, nil
	}

	stmt, params, err := r.PrepareFindTreeByDN(baseDN)
	if err != nil {
		return 0, nil, err
	}

	rows, err := stmt.Queryx(params)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()

	cid := []int64{}

	var id int64
	var dnOrig string
	for rows.Next() {
		err := rows.Scan(&id, &dnOrig)
		if err != nil {
			return 0, nil, xerrors.Errorf("Failed to scan id, dnOrig. err: %w", err)
		}
		cid = append(cid, id)
		dnOrigCache[id] = dnOrig
	}

	if len(cid) == 0 {
		// Need to return success response
		return 0, nil, NewSuccess()
	}
	if len(cid) == 1 {
		return cid[0], []int64{}, nil
	}
	return cid[0], cid[1:], nil
}
