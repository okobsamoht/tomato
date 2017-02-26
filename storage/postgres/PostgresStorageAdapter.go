package postgres

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"regexp"

	"github.com/lfq7413/tomato/errs"
	"github.com/lfq7413/tomato/types"
	"github.com/lfq7413/tomato/utils"
	"github.com/lib/pq"
)

const postgresSchemaCollectionName = "_SCHEMA"

const postgresRelationDoesNotExistError = "42P01"
const postgresDuplicateRelationError = "42P07"
const postgresDuplicateColumnError = "42701"
const postgresUniqueIndexViolationError = "23505"
const postgresTransactionAbortedError = "25P02"

// PostgresAdapter postgres 数据库适配器
type PostgresAdapter struct {
	collectionPrefix string
	collectionList   []string
	db               *sql.DB
}

// NewPostgresAdapter ...
func NewPostgresAdapter(collectionPrefix string, db *sql.DB) *PostgresAdapter {
	return &PostgresAdapter{
		collectionPrefix: collectionPrefix,
		collectionList:   []string{},
		db:               db,
	}
}

// ensureSchemaCollectionExists 确保 _SCHEMA 表存在，不存在则创建表
func (p *PostgresAdapter) ensureSchemaCollectionExists() error {
	_, err := p.db.Exec(`CREATE TABLE IF NOT EXISTS "_SCHEMA" ( "className" varChar(120), "schema" jsonb, "isParseClass" bool, PRIMARY KEY ("className") )`)
	if err != nil {
		if e, ok := err.(*pq.Error); ok {
			if e.Code == postgresDuplicateRelationError || e.Code == postgresUniqueIndexViolationError {
				// _SCHEMA 表已经存在，已经由其他请求创建，忽略错误
				return nil
			}
		} else {
			return err
		}
	}
	return nil
}

// ClassExists 检测数据库中是否存在指定类
func (p *PostgresAdapter) ClassExists(name string) bool {
	var result bool
	err := p.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM   information_schema.tables WHERE table_name = $1)`, name).Scan(&result)
	if err != nil {
		return false
	}
	return result
}

// SetClassLevelPermissions 设置类级别权限
func (p *PostgresAdapter) SetClassLevelPermissions(className string, CLPs types.M) error {
	err := p.ensureSchemaCollectionExists()
	if err != nil {
		return err
	}
	if CLPs == nil {
		CLPs = types.M{}
	}
	b, err := json.Marshal(CLPs)
	if err != nil {
		return err
	}

	qs := `UPDATE "_SCHEMA" SET "schema" = json_object_set_key("schema", $1::text, $2::jsonb) WHERE "className"=$3 `
	_, err = p.db.Exec(qs, "classLevelPermissions", string(b), className)
	if err != nil {
		return err
	}

	return nil
}

// CreateClass 创建类
func (p *PostgresAdapter) CreateClass(className string, schema types.M) (types.M, error) {
	b, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}

	err = p.createTable(className, schema)
	if err != nil {
		return nil, err
	}

	_, err = p.db.Exec(`INSERT INTO "_SCHEMA" ("className", "schema", "isParseClass") VALUES ($1, $2, $3)`, className, string(b), true)
	if err != nil {
		if e, ok := err.(*pq.Error); ok {
			if e.Code == postgresUniqueIndexViolationError {
				return nil, errs.E(errs.DuplicateValue, "Class "+className+" already exists.")
			}
		}
		return nil, err
	}

	return toParseSchema(schema), nil
}

// createTable 仅创建表，不加入 schema 中
func (p *PostgresAdapter) createTable(className string, schema types.M) error {
	if schema == nil {
		schema = types.M{}
	}
	valuesArray := types.S{}
	patternsArray := []string{}
	var fields types.M
	if f := utils.M(schema["fields"]); f != nil {
		fields = f
	}

	if className == "_User" {
		fields["_email_verify_token_expires_at"] = types.M{"type": "Date"}
		fields["_email_verify_token"] = types.M{"type": "String"}
		fields["_account_lockout_expires_at"] = types.M{"type": "Date"}
		fields["_failed_login_count"] = types.M{"type": "Number"}
		fields["_perishable_token"] = types.M{"type": "String"}
		fields["_perishable_token_expires_at"] = types.M{"type": "Date"}
		fields["_password_changed_at"] = types.M{"type": "Date"}
		fields["_password_history"] = types.M{"type": "Array"}
	}

	relations := []string{}

	for fieldName, t := range fields {
		parseType := utils.M(t)
		if parseType == nil {
			parseType = types.M{}
		}

		if utils.S(parseType["type"]) == "Relation" {
			relations = append(relations, fieldName)
			continue
		}

		if fieldName == "_rperm" || fieldName == "_wperm" {
			parseType["contents"] = types.M{"type": "String"}
		}

		valuesArray = append(valuesArray, fieldName)
		postgresType, err := parseTypeToPostgresType(parseType)
		if err != nil {
			return err
		}
		valuesArray = append(valuesArray, postgresType)

		patternsArray = append(patternsArray, `"%s" %s`)
		if fieldName == "objectId" {
			valuesArray = append(valuesArray, fieldName)
			patternsArray = append(patternsArray, `PRIMARY KEY ("%s")`)
		}
	}

	qs := `CREATE TABLE IF NOT EXISTS "%s" (` + strings.Join(patternsArray, ",") + `)`
	values := append(types.S{className}, valuesArray...)
	qs = fmt.Sprintf(qs, values...)

	err := p.ensureSchemaCollectionExists()
	if err != nil {
		return err
	}

	_, err = p.db.Exec(qs)
	if err != nil {
		if e, ok := err.(*pq.Error); ok {
			if e.Code == postgresDuplicateRelationError {
				// 表已经存在，已经由其他请求创建，忽略错误
			} else {
				return err
			}
		} else {
			return err
		}
	}

	// 创建 relation 表
	for _, fieldName := range relations {
		name := fmt.Sprintf(`_Join:%s:%s`, fieldName, className)
		_, err = p.db.Exec(fmt.Sprintf(`CREATE TABLE IF NOT EXISTS "%s" ("relatedId" varChar(120), "owningId" varChar(120), PRIMARY KEY("relatedId", "owningId") )`, name))
		if err != nil {
			return err
		}
	}

	return nil
}

// AddFieldIfNotExists 添加字段定义
func (p *PostgresAdapter) AddFieldIfNotExists(className, fieldName string, fieldType types.M) error {
	if fieldType == nil {
		fieldType = types.M{}
	}

	if utils.S(fieldType["type"]) != "Relation" {
		tp, err := parseTypeToPostgresType(fieldType)
		if err != nil {
			return err
		}
		qs := fmt.Sprintf(`ALTER TABLE "%s" ADD COLUMN "%s" %s`, className, fieldName, tp)
		_, err = p.db.Exec(qs)
		if err != nil {
			if e, ok := err.(*pq.Error); ok {
				if e.Code == postgresRelationDoesNotExistError {
					// TODO 添加默认字段
					_, ce := p.CreateClass(className, types.M{"fields": types.M{fieldName: fieldType}})
					if ce != nil {
						return ce
					}
				} else if e.Code == postgresDuplicateColumnError {
					// Column 已经存在，由其他请求创建
				} else {
					return err
				}
			} else {
				return err
			}
		}
	} else {
		name := fmt.Sprintf(`_Join:%s:%s`, fieldName, className)
		qs := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS "%s" ("relatedId" varChar(120), "owningId" varChar(120), PRIMARY KEY("relatedId", "owningId") )`, name)
		_, err := p.db.Exec(qs)
		if err != nil {
			return err
		}
	}

	qs := `SELECT "schema" FROM "_SCHEMA" WHERE "className" = $1`
	rows, err := p.db.Query(qs, className)
	if err != nil {
		return err
	}
	if rows.Next() {
		var sch types.M
		var v []byte
		err := rows.Scan(&v)
		if err != nil {
			return err
		}
		err = json.Unmarshal(v, &sch)
		if err != nil {
			return err
		}
		if sch == nil {
			sch = types.M{}
		}
		var fields types.M
		if v := utils.M(sch["fields"]); v != nil {
			fields = v
		} else {
			fields = types.M{}
		}
		if _, ok := fields[fieldName]; ok {
			// 当表不存在时，会进行新建表，所以也会走到这里，不再处理错误
			// Attempted to add a field that already exists
			return nil
		}
		fields[fieldName] = fieldType
		sch["fields"] = fields
		b, err := json.Marshal(sch)
		qs := `UPDATE "_SCHEMA" SET "schema"=$1 WHERE "className"=$2`
		_, err = p.db.Exec(qs, b, className)
		if err != nil {
			return err
		}
	}

	return nil
}

// DeleteClass 删除指定表
func (p *PostgresAdapter) DeleteClass(className string) (types.M, error) {
	qs := fmt.Sprintf(`DROP TABLE IF EXISTS "%s"`, className)
	_, err := p.db.Exec(qs)
	if err != nil {
		return nil, err
	}

	qs = `DELETE FROM "_SCHEMA" WHERE "className"=$1`
	_, err = p.db.Exec(qs, className)
	if err != nil {
		return nil, err
	}

	return types.M{}, nil
}

// DeleteAllClasses 删除所有表，仅用于测试
func (p *PostgresAdapter) DeleteAllClasses() error {
	qs := `SELECT "className","schema" FROM "_SCHEMA"`
	rows, err := p.db.Query(qs)
	if err != nil {
		if e, ok := err.(*pq.Error); ok && e.Code == postgresRelationDoesNotExistError {
			// _SCHEMA 不存在，则不删除
			return nil
		}
		return err
	}

	classNames := []string{}
	schemas := []types.M{}

	for rows.Next() {
		var clsName string
		var sch types.M
		var v []byte
		err := rows.Scan(&clsName, &v)
		if err != nil {
			return err
		}
		err = json.Unmarshal(v, &sch)
		if err != nil {
			return err
		}
		classNames = append(classNames, clsName)
		schemas = append(schemas, sch)
	}

	joins := []string{}
	for _, sch := range schemas {
		joins = append(joins, joinTablesForSchema(sch)...)
	}

	classes := []string{"_SCHEMA", "_PushStatus", "_JobStatus", "_Hooks", "_GlobalConfig"}
	classes = append(classes, classNames...)
	classes = append(classes, joins...)

	for _, name := range classes {
		qs = fmt.Sprintf(`DROP TABLE IF EXISTS "%s"`, name)
		p.db.Exec(qs)
	}

	return nil
}

// DeleteFields 删除字段
func (p *PostgresAdapter) DeleteFields(className string, schema types.M, fieldNames []string) error {
	if schema == nil {
		schema = types.M{}
	}

	fields := utils.M(schema["fields"])
	if fields == nil {
		fields = types.M{}
	}
	fldNames := types.S{}
	for _, fieldName := range fieldNames {
		field := utils.M(fields[fieldName])
		if field != nil && utils.S(field["type"]) == "Relation" {
			// 不处理 Relation 类型字段
		} else {
			fldNames = append(fldNames, fieldName)
		}
		delete(fields, fieldName)
	}
	schema["fields"] = fields

	values := append(types.S{className}, fldNames...)
	columnArray := []string{}
	for _ = range fldNames {
		columnArray = append(columnArray, `"%s"`)
	}
	columns := strings.Join(columnArray, ",")

	b, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	qs := `UPDATE "_SCHEMA" SET "schema"=$1 WHERE "className"=$2`
	_, err = p.db.Exec(qs, b, className)
	if err != nil {
		return err
	}

	if len(values) > 1 {
		qs = fmt.Sprintf(`ALTER TABLE "%%s" DROP COLUMN %s`, columns)
		qs = fmt.Sprintf(qs, values...)
		_, err = p.db.Exec(qs)
		if err != nil {
			return err
		}
	}

	return nil
}

// CreateObject 创建对象
func (p *PostgresAdapter) CreateObject(className string, schema, object types.M) error {
	columnsArray := types.S{}
	valuesArray := types.S{}
	schema = toPostgresSchema(schema)
	object = handleDotFields(object)

	err := validateKeys(object)
	if err != nil {
		return err
	}

	for fieldName := range object {
		re := regexp.MustCompile(`^_auth_data_([a-zA-Z0-9_]+)$`)
		authDataMatch := re.FindStringSubmatch(fieldName)
		if authDataMatch != nil && len(authDataMatch) == 2 {
			provider := authDataMatch[1]
			authData := utils.M(object["authData"])
			if authData == nil {
				authData = types.M{}
			}
			authData[provider] = object[fieldName]
			delete(object, fieldName)
			fieldName = "authData"
			object["authData"] = authData
		}
		columnsArray = append(columnsArray, fieldName)

		fields := utils.M(schema["fields"])
		if fields == nil {
			fields = types.M{}
		}
		if fields[fieldName] == nil && className == "_User" {
			if fieldName == "_email_verify_token" ||
				fieldName == "_failed_login_count" ||
				fieldName == "_perishable_token" ||
				fieldName == "_password_history" {
				valuesArray = append(valuesArray, object[fieldName])
			}

			if fieldName == "_email_verify_token_expires_at" {
				if v := utils.M(object[fieldName]); v != nil && utils.S(v["iso"]) != "" {
					valuesArray = append(valuesArray, v["iso"])
				} else {
					valuesArray = append(valuesArray, nil)
				}
			}

			if fieldName == "_account_lockout_expires_at" ||
				fieldName == "_perishable_token_expires_at" ||
				fieldName == "_password_changed_at" {
				if v := utils.M(object[fieldName]); v != nil && utils.S(v["iso"]) != "" {
					valuesArray = append(valuesArray, v["iso"])
				} else {
					valuesArray = append(valuesArray, nil)
				}
			}

			continue
		}
		// TODO
	}

	return nil
}

// GetAllClasses ...
func (p *PostgresAdapter) GetAllClasses() ([]types.M, error) {
	err := p.ensureSchemaCollectionExists()
	if err != nil {
		return nil, err
	}
	qs := `SELECT "className","schema" FROM "_SCHEMA"`
	rows, err := p.db.Query(qs)
	if err != nil {
		return nil, err
	}

	schemas := []types.M{}

	for rows.Next() {
		var clsName string
		var sch types.M
		var v []byte
		err := rows.Scan(&clsName, &v)
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(v, &sch)
		if err != nil {
			return nil, err
		}
		sch["className"] = clsName
		schemas = append(schemas, toParseSchema(sch))
	}

	return schemas, nil
}

// GetClass ...
func (p *PostgresAdapter) GetClass(className string) (types.M, error) {
	qs := `SELECT "schema" FROM "_SCHEMA" WHERE "className"=$1`
	rows, err := p.db.Query(qs, className)
	if err != nil {
		return nil, err
	}

	schema := types.M{}
	if rows.Next() {
		var v []byte
		err = rows.Scan(&v)
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(v, &schema)
		if err != nil {
			return nil, err
		}
	} else {
		return schema, nil
	}

	return toParseSchema(schema), nil
}

// DeleteObjectsByQuery ...
func (p *PostgresAdapter) DeleteObjectsByQuery(className string, schema, query types.M) error {
	// TODO
	// buildWhereClause
	return nil
}

// Find ...
func (p *PostgresAdapter) Find(className string, schema, query, options types.M) ([]types.M, error) {
	// TODO
	// buildWhereClause
	return nil, nil
}

// Count ...
func (p *PostgresAdapter) Count(className string, schema, query types.M) (int, error) {
	// TODO
	// buildWhereClause
	return 0, nil
}

// UpdateObjectsByQuery ...
func (p *PostgresAdapter) UpdateObjectsByQuery(className string, schema, query, update types.M) error {
	// TODO
	// buildWhereClause
	// jsonObjectSetKey
	// arrayAdd
	// arrayAddUnique
	// arrayRemove

	return nil
}

// FindOneAndUpdate ...
func (p *PostgresAdapter) FindOneAndUpdate(className string, schema, query, update types.M) (types.M, error) {
	// TODO
	// UpdateObjectsByQuery
	return nil, nil
}

// UpsertOneObject ...
func (p *PostgresAdapter) UpsertOneObject(className string, schema, query, update types.M) error {
	// TODO
	// createObject
	// FindOneAndUpdate
	return nil
}

// EnsureUniqueness ...
func (p *PostgresAdapter) EnsureUniqueness(className string, schema types.M, fieldNames []string) error {
	// TODO
	return nil
}

// PerformInitialization ...
func (p *PostgresAdapter) PerformInitialization(options types.M) error {
	if options == nil {
		options = types.M{}
	}

	if volatileClassesSchemas, ok := options["VolatileClassesSchemas"].([]types.M); ok {
		for _, schema := range volatileClassesSchemas {
			err := p.createTable(utils.S(schema["className"]), schema)
			if err != nil {
				if e, ok := err.(*pq.Error); ok {
					if e.Code != postgresDuplicateRelationError {
						return err
					}
				} else if e, ok := err.(*errs.TomatoError); ok {
					if e.Code != errs.InvalidClassName {
						return err
					}
				} else {
					return err
				}
			}
		}
	}

	_, err := p.db.Exec(jsonObjectSetKey)
	if err != nil {
		return err
	}

	_, err = p.db.Exec(arrayAdd)
	if err != nil {
		return err
	}

	_, err = p.db.Exec(arrayAddUnique)
	if err != nil {
		return err
	}

	_, err = p.db.Exec(arrayRemove)
	if err != nil {
		return err
	}

	_, err = p.db.Exec(arrayContainsAll)
	if err != nil {
		return err
	}

	_, err = p.db.Exec(arrayContains)
	if err != nil {
		return err
	}

	return nil
}

var parseToPosgresComparator = map[string]string{
	"$gt":  ">",
	"$lt":  "<",
	"$gte": ">=",
	"$lte": "<=",
}

func parseTypeToPostgresType(t types.M) (string, error) {
	if t == nil {
		return "", nil
	}
	tp := utils.S(t["type"])
	switch tp {
	case "String":
		return "text", nil
	case "Date":
		return "timestamp with time zone", nil
	case "Object":
		return "jsonb", nil
	case "File":
		return "text", nil
	case "Boolean":
		return "boolean", nil
	case "Pointer":
		return "char(24)", nil
	case "Number":
		return "double precision", nil
	case "GeoPoint":
		return "point", nil
	case "Array":
		if contents := utils.M(t["contents"]); contents != nil {
			if utils.S(contents["type"]) == "String" {
				return "text[]", nil
			}
		}
		return "jsonb", nil
	default:
		return "", errs.E(errs.IncorrectType, "no type for "+tp+" yet")
	}
}

func toPostgresValue(value interface{}) interface{} {
	if v := utils.M(value); v != nil {
		if utils.S(v["__type"]) == "Date" {
			return v["iso"]
		}
		if utils.S(v["__type"]) == "File" {
			return v["name"]
		}
	}
	return value
}

func transformValue(value interface{}) interface{} {
	if v := utils.M(value); v != nil {
		if utils.S(v["__type"]) == "Pointer" {
			return v["objectId"]
		}
	}
	return value
}

var emptyCLPS = types.M{
	"find":     types.M{},
	"get":      types.M{},
	"create":   types.M{},
	"update":   types.M{},
	"delete":   types.M{},
	"addField": types.M{},
}

var defaultCLPS = types.M{
	"find":     types.M{"*": true},
	"get":      types.M{"*": true},
	"create":   types.M{"*": true},
	"update":   types.M{"*": true},
	"delete":   types.M{"*": true},
	"addField": types.M{"*": true},
}

func toParseSchema(schema types.M) types.M {
	if schema == nil {
		return nil
	}

	var fields types.M
	if fields = utils.M(schema["fields"]); fields == nil {
		fields = types.M{}
	}

	if utils.S(schema["className"]) == "_User" {
		if _, ok := fields["_hashed_password"]; ok {
			delete(fields, "_hashed_password")
		}
	}

	if _, ok := fields["_wperm"]; ok {
		delete(fields, "_wperm")
	}
	if _, ok := fields["_rperm"]; ok {
		delete(fields, "_rperm")
	}

	var clps types.M
	clps = utils.CopyMap(defaultCLPS)
	if classLevelPermissions := utils.M(schema["classLevelPermissions"]); classLevelPermissions != nil {
		// clps = utils.CopyMap(emptyCLPS)
		// 不存在的 action 默认为公共权限
		for k, v := range classLevelPermissions {
			clps[k] = v
		}
	}

	return types.M{
		"className":             schema["className"],
		"fields":                fields,
		"classLevelPermissions": clps,
	}
}

func toPostgresSchema(schema types.M) types.M {
	if schema == nil {
		return nil
	}

	var fields types.M
	if fields = utils.M(schema["fields"]); fields == nil {
		fields = types.M{}
	}

	fields["_wperm"] = types.M{
		"type":     "Array",
		"contents": types.M{"type": "String"},
	}
	fields["_rperm"] = types.M{
		"type":     "Array",
		"contents": types.M{"type": "String"},
	}

	if utils.S(schema["className"]) == "_User" {
		fields["_hashed_password"] = types.M{"type": "String"}
		fields["_password_history"] = types.M{"type": "Array"}
	}

	schema["fields"] = fields

	return schema
}

func handleDotFields(object types.M) types.M {
	for fieldName := range object {
		if strings.Index(fieldName, ".") == -1 {
			continue
		}
		components := strings.Split(fieldName, ".")

		value := object[fieldName]
		if v := utils.M(value); v != nil {
			if utils.S(v["__op"]) == "Delete" {
				value = nil
			}
		}

		currentObj := object
		for i, next := range components {
			if i == (len(components) - 1) {
				currentObj[next] = value
				break
			}
			obj := currentObj[next]
			if obj == nil {
				obj = types.M{}
				currentObj[next] = obj
			}
			currentObj = utils.M(currentObj[next])
		}

		delete(object, fieldName)
	}
	return object
}

func validateKeys(object interface{}) error {
	if obj := utils.M(object); obj != nil {
		for key, value := range obj {
			err := validateKeys(value)
			if err != nil {
				return err
			}

			if strings.Contains(key, "$") || strings.Contains(key, ".") {
				return errs.E(errs.InvalidNestedKey, "Nested keys should not contain the '$' or '.' characters")
			}
		}
	}
	return nil
}

func joinTablesForSchema(schema types.M) []string {
	list := []string{}
	if schema != nil {
		if fields := utils.M(schema["fields"]); fields != nil {
			className := utils.S(schema["className"])
			for field, v := range fields {
				if tp := utils.M(v); tp != nil {
					if utils.S(tp["type"]) == "Relation" {
						list = append(list, "_Join:"+field+":"+className)
					}
				}
			}
		}
	}
	return list
}

type whereClause struct {
	pattern string
	values  types.S
	sorts   []string
}

func buildWhereClause(schema, query types.M, index int) (*whereClause, error) {
	// arrayContainsAll
	// arrayContains
	patterns := []string{}
	values := types.S{}
	sorts := []string{}

	schema = toPostgresSchema(schema)
	if schema == nil {
		schema = types.M{}
	}
	fields := utils.M(schema["fields"])
	if fields == nil {
		fields = types.M{}
	}
	for fieldName, fieldValue := range query {
		isArrayField := false
		if fields != nil {
			if tp := utils.M(fields[fieldName]); tp != nil {
				if utils.S(tp["type"]) == "Array" {
					isArrayField = true
				}
			}
		}
		initialPatternsLength := len(patterns)

		if fields[fieldName] == nil {
			if v := utils.M(fieldValue); v != nil {
				if b, ok := v["$exists"].(bool); ok && b == false {
					continue
				}
			}
		}

		if strings.Contains(fieldName, ".") {
			components := strings.Split(fieldName, ".")
			for index, cmpt := range components {
				if index == 0 {
					components[index] = `"` + cmpt + `"`
				} else {
					components[index] = `'` + cmpt + `'`
				}
			}
			name := strings.Join(components[:len(components)-1], "->")
			name = name + "->>" + components[len(components)-1]
			patterns = append(patterns, fmt.Sprintf(`%s = '%v'`, name, fieldValue))
		} else if _, ok := fieldValue.(string); ok {
			patterns = append(patterns, fmt.Sprintf(`$%d:name = $%d`, index, index+1))
			values = append(values, fieldName, fieldValue)
			index = index + 2
		} else if _, ok := fieldValue.(bool); ok {
			patterns = append(patterns, fmt.Sprintf(`$%d:name = $%d`, index, index+1))
			values = append(values, fieldName, fieldValue)
			index = index + 2
		} else if _, ok := fieldValue.(float64); ok {
			patterns = append(patterns, fmt.Sprintf(`$%d:name = $%d`, index, index+1))
			values = append(values, fieldName, fieldValue)
			index = index + 2
		} else if _, ok := fieldValue.(int); ok {
			patterns = append(patterns, fmt.Sprintf(`$%d:name = $%d`, index, index+1))
			values = append(values, fieldName, fieldValue)
			index = index + 2
		} else if fieldName == "$or" || fieldName == "$and" {
			clauses := []string{}
			clauseValues := types.S{}
			if array := utils.A(fieldValue); array != nil {
				for _, v := range array {
					if subQuery := utils.M(v); subQuery != nil {
						clause, err := buildWhereClause(schema, subQuery, index)
						if err != nil {
							return nil, err
						}
						if len(clause.pattern) > 0 {
							clauses = append(clauses, clause.pattern)
							clauseValues = append(clauseValues, clause.values...)
							index = index + len(clause.values)
						}
					}
				}
			}
			var orOrAnd string
			if fieldName == "$or" {
				orOrAnd = " OR "
			} else {
				orOrAnd = " AND "
			}
			patterns = append(patterns, fmt.Sprintf(`(%s)`, strings.Join(clauses, orOrAnd)))
			values = append(values, clauseValues...)
		}

		if value := utils.M(fieldValue); value != nil {

			if v, ok := value["$ne"]; ok {
				if isArrayField {
					j, _ := json.Marshal(types.S{v})
					value["$ne"] = string(j)
					patterns = append(patterns, fmt.Sprintf(`NOT array_contains($%d:name, $%d)`, index, index+1))
				} else {
					if v == nil {
						patterns = append(patterns, fmt.Sprintf(`$%d:name <> $%d`, index, index+1))
					} else {
						patterns = append(patterns, fmt.Sprintf(`($%d:name <> $%d OR $%d:name IS NULL)`, index, index+1, index))
					}
				}

				values = append(values, fieldName, value["$ne"])
				index = index + 2
			}

			if v, ok := value["$eq"]; ok {
				patterns = append(patterns, fmt.Sprintf(`$%d:name = $%d`, index, index+1))
				values = append(values, fieldName, v)
				index = index + 2
			}

			inArray := utils.A(value["$in"])
			ninArray := utils.A(value["$nin"])
			isInOrNin := (inArray != nil) || (ninArray != nil)
			isTypeString := false
			if tp := utils.M(fields[fieldName]); tp != nil {
				if contents := utils.M(tp["contents"]); contents != nil {
					if utils.S(contents["type"]) == "String" {
						isTypeString = true
					}
				}
			}
			if inArray != nil && isArrayField && isTypeString {
				inPatterns := []string{}
				allowNull := false
				values = append(values, fieldName)

				for listIndex, listElem := range inArray {
					if listElem == nil {
						allowNull = true
					} else {
						values = append(values, listElem)
						i := 0
						if allowNull {
							i = index + 1 + listIndex - 1
						} else {
							i = index + 1 + listIndex
						}
						inPatterns = append(inPatterns, fmt.Sprintf("$%d", i))
					}
				}

				if allowNull {
					patterns = append(patterns, fmt.Sprintf(`($%d:name IS NULL OR $%d:name && ARRAY[%s])`, index, index, strings.Join(inPatterns, ",")))
				} else {
					patterns = append(patterns, fmt.Sprintf(`($%d:name && ARRAY[%s])`, index, strings.Join(inPatterns, ",")))
				}
				index = index + 1 + len(inPatterns)
			} else if isInOrNin {
				createConstraint := func(baseArray types.S, notIn bool) {
					if len(baseArray) > 0 {
						not := ""
						if notIn {
							not = " NOT "
						}
						if isArrayField {
							patterns = append(patterns, fmt.Sprintf("%s array_contains($%d:name, $%d)", not, index, index+1))
							j, _ := json.Marshal(baseArray)
							values = append(values, fieldName, string(j))
							index = index + 2
						} else {
							inPatterns := []string{}
							values = append(values, fieldName)
							for listIndex, listElem := range baseArray {
								values = append(values, listElem)
								inPatterns = append(inPatterns, fmt.Sprintf("$%d", index+1+listIndex))
							}
							patterns = append(patterns, fmt.Sprintf("$%d:name %s IN (%s)", index, not, strings.Join(inPatterns, ",")))
							index = index + 1 + len(inPatterns)
						}
					} else if !notIn {
						values = append(values, fieldName)
						patterns = append(patterns, fmt.Sprintf("$%d:name IS NULL", index))
						index = index + 1
					}
				}
				if inArray != nil {
					createConstraint(inArray, false)
				}
				if ninArray != nil {
					createConstraint(ninArray, true)
				}
			}

			allArray := utils.A(value["$all"])
			if allArray != nil && isArrayField {
				patterns = append(patterns, fmt.Sprintf("array_contains_all($%d:name, $%d::jsonb)", index, index+1))
				j, _ := json.Marshal(allArray)
				values = append(values, fieldName, string(j))
				index = index + 2
			}

			if b, ok := value["$exists"].(bool); ok {
				if b {
					patterns = append(patterns, fmt.Sprintf("$%d:name IS NOT NULL", index))
				} else {
					patterns = append(patterns, fmt.Sprintf("$%d:name IS NULL", index))
				}
				values = append(values, fieldName)
				index = index + 1
			}

			if point := utils.M(value["$nearSphere"]); point != nil {
				var distance float64
				if v, ok := value["$maxDistance"].(float64); ok {
					distance = v
				}
				distanceInKM := distance * 6371 * 1000
				patterns = append(patterns, fmt.Sprintf("ST_distance_sphere($%d:name::geometry, POINT($%d, $%d)::geometry) <= $%d", index, index+1, index+2, index+3))
				sorts = append(sorts, fmt.Sprintf("ST_distance_sphere($%d:name::geometry, POINT($%d, $%d)::geometry) ASC", index, index+1, index+2))
				values = append(values, fieldName, point["longitude"], point["latitude"], distanceInKM)
				index = index + 4
			}

			if within := utils.M(value["$within"]); within != nil {
				if box := utils.A(within["$box"]); len(box) == 2 {
					box1 := utils.M(box[0])
					box2 := utils.M(box[1])
					if box1 != nil && box2 != nil {
						left := box1["longitude"]
						bottom := box1["latitude"]
						right := box2["longitude"]
						top := box2["latitude"]

						patterns = append(patterns, fmt.Sprintf("$%d:name::point <@ $%d::box", index, index+1))
						values = append(values, fieldName, fmt.Sprintf("((%v, %v), (%v, %v))", left, bottom, right, top))
						index = index + 2
					}
				}
			}

			if regex := utils.S(value["$regex"]); regex != "" {
				operator := "~"
				opts := utils.S(value["$options"])
				if opts != "" {
					if strings.Contains(opts, "i") {
						operator = "~*"
					}
					if strings.Contains(opts, "x") {
						regex = removeWhiteSpace(regex)
					}
				}

				regex = processRegexPattern(regex)

				patterns = append(patterns, fmt.Sprintf(`$%d:name %s '$%d:raw'`, index, operator, index+1))
				values = append(values, fieldName, regex)
				index = index + 2
			}

			if utils.S(value["__type"]) == "Pointer" {
				if isArrayField {
					patterns = append(patterns, fmt.Sprintf(`array_contains($%d:name, $%d)`, index, index+1))
					j, _ := json.Marshal(types.S{value})
					values = append(values, fieldName, string(j))
					index = index + 2
				} else {
					patterns = append(patterns, fmt.Sprintf(`$%d:name = $%d`, index, index+1))
					values = append(values, fieldName, value["objectId"])
					index = index + 2
				}
			}

			if utils.S(value["__type"]) == "Date" {
				patterns = append(patterns, fmt.Sprintf(`$%d:name = $%d`, index, index+1))
				values = append(values, fieldName, value["iso"])
				index = index + 2
			}

			for cmp, pgComparator := range parseToPosgresComparator {
				if v, ok := value[cmp]; ok {
					patterns = append(patterns, fmt.Sprintf(`$%d:name %s $%d`, index, pgComparator, index+1))
					values = append(values, fieldName, toPostgresValue(v))
					index = index + 2
				}
			}
		}

		if initialPatternsLength == len(patterns) {
			s, _ := json.Marshal(fieldValue)
			return nil, errs.E(errs.OperationForbidden, "Postgres doesn't support this query type yet "+string(s))
		}
	}
	for i, v := range values {
		values[i] = transformValue(v)
	}
	return &whereClause{strings.Join(patterns, " AND "), values, sorts}, nil
}

func removeWhiteSpace(s string) string {
	if strings.HasSuffix(s, "\n") == false {
		s = s + "\n"
	}

	re := regexp.MustCompile(`(?im)^#.*\n`)
	s = re.ReplaceAllString(s, "")
	re = regexp.MustCompile(`(?im)([^\\])#.*\n`)
	s = re.ReplaceAllString(s, "$1")
	re = regexp.MustCompile(`(?im)([^\\])\s+`)
	s = re.ReplaceAllString(s, "$1")
	re = regexp.MustCompile(`^\s+`)
	s = re.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)

	return s
}

func processRegexPattern(s string) string {
	if strings.HasPrefix(s, "^") {
		return "^" + literalizeRegexPart(s[1:])
	} else if strings.HasSuffix(s, "$") {
		return literalizeRegexPart(s[:len(s)-1]) + "$"
	}
	return literalizeRegexPart(s)
}

func createLiteralRegex(s string) string {
	chars := strings.Split(s, "")
	for i, c := range chars {
		if m, _ := regexp.MatchString(`[0-9a-zA-Z]`, c); m == false {
			if c == `'` {
				chars[i] = `''`
			} else {
				chars[i] = `\` + c
			}
		}
	}
	return strings.Join(chars, "")
}

func literalizeRegexPart(s string) string {
	// go 不支持 (?!) 语法，需要进行等价替换
	// /\\Q((?!\\E).*)\\E$/
	// /\\Q(\\[^E\n\r].*|[^\\\n\r].*|.??)\\E$/
	matcher1 := regexp.MustCompile(`\\Q(\\[^E\n\r].*|[^\\\n\r].*|.??)\\E$`)
	result1 := matcher1.FindStringSubmatch(s)
	if len(result1) > 1 {
		index := strings.Index(s, result1[0])
		prefix := s[:index]
		remaining := result1[1]
		return literalizeRegexPart(prefix) + createLiteralRegex(remaining)
	}

	// /\\Q((?!\\E).*)$/
	// /\\Q(\\[^E\n\r].*|[^\\\n\r].*|.??)$/
	matcher2 := regexp.MustCompile(`\\Q(\\[^E\n\r].*|[^\\\n\r].*|.??)$`)
	result2 := matcher2.FindStringSubmatch(s)
	if len(result2) > 1 {
		index := strings.Index(s, result2[0])
		prefix := s[:index]
		remaining := result2[1]
		return literalizeRegexPart(prefix) + createLiteralRegex(remaining)
	}

	re := regexp.MustCompile(`([^\\])(\\E)`)
	s = re.ReplaceAllString(s, "$1")
	re = regexp.MustCompile(`([^\\])(\\Q)`)
	s = re.ReplaceAllString(s, "$1")
	re = regexp.MustCompile(`^\\E`)
	s = re.ReplaceAllString(s, "")
	re = regexp.MustCompile(`^\\Q`)
	s = re.ReplaceAllString(s, "")
	re = regexp.MustCompile(`([^'])'`)
	s = re.ReplaceAllString(s, "$1''")
	re = regexp.MustCompile(`^'([^'])`)
	s = re.ReplaceAllString(s, "''$1")
	return s
}

// Function to set a key on a nested JSON document
const jsonObjectSetKey = `CREATE OR REPLACE FUNCTION "json_object_set_key"(
  "json"          jsonb,
  "key_to_set"    TEXT,
  "value_to_set"  anyelement
)
  RETURNS jsonb 
  LANGUAGE sql 
  IMMUTABLE 
  STRICT 
AS $function$
SELECT concat('{', string_agg(to_json("key") || ':' || "value", ','), '}')::jsonb
  FROM (SELECT *
          FROM jsonb_each("json")
         WHERE "key" <> "key_to_set"
         UNION ALL
        SELECT "key_to_set", to_json("value_to_set")::jsonb) AS "fields"
$function$`

const arrayAdd = `CREATE OR REPLACE FUNCTION "array_add"(
  "array"   jsonb,
  "values"  jsonb
)
  RETURNS jsonb 
  LANGUAGE sql 
  IMMUTABLE 
  STRICT 
AS $function$ 
  SELECT array_to_json(ARRAY(SELECT unnest(ARRAY(SELECT DISTINCT jsonb_array_elements("array")) ||  ARRAY(SELECT jsonb_array_elements("values")))))::jsonb;
$function$`

const arrayAddUnique = `CREATE OR REPLACE FUNCTION "array_add_unique"(
  "array"   jsonb,
  "values"  jsonb
)
  RETURNS jsonb 
  LANGUAGE sql 
  IMMUTABLE 
  STRICT 
AS $function$ 
  SELECT array_to_json(ARRAY(SELECT DISTINCT unnest(ARRAY(SELECT DISTINCT jsonb_array_elements("array")) ||  ARRAY(SELECT DISTINCT jsonb_array_elements("values")))))::jsonb;
$function$`

const arrayRemove = `CREATE OR REPLACE FUNCTION "array_remove"(
  "array"   jsonb,
  "values"  jsonb
)
  RETURNS jsonb 
  LANGUAGE sql 
  IMMUTABLE 
  STRICT 
AS $function$ 
  SELECT array_to_json(ARRAY(SELECT * FROM jsonb_array_elements("array") as elt WHERE elt NOT IN (SELECT * FROM (SELECT jsonb_array_elements("values")) AS sub)))::jsonb;
$function$`

const arrayContainsAll = `CREATE OR REPLACE FUNCTION "array_contains_all"(
  "array"   jsonb,
  "values"  jsonb
)
  RETURNS boolean 
  LANGUAGE sql 
  IMMUTABLE 
  STRICT 
AS $function$ 
  SELECT RES.CNT = jsonb_array_length("values") FROM (SELECT COUNT(*) as CNT FROM jsonb_array_elements("array") as elt WHERE elt IN (SELECT jsonb_array_elements("values"))) as RES ;
$function$`

const arrayContains = `CREATE OR REPLACE FUNCTION "array_contains"(
  "array"   jsonb,
  "values"  jsonb
)
  RETURNS boolean 
  LANGUAGE sql 
  IMMUTABLE 
  STRICT 
AS $function$ 
  SELECT RES.CNT >= 1 FROM (SELECT COUNT(*) as CNT FROM jsonb_array_elements("array") as elt WHERE elt IN (SELECT jsonb_array_elements("values"))) as RES ;
$function$`
