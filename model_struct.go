package gorm

import (
	"database/sql"
	"fmt"
	"go/ast"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qor/inflection"
)

type modelStructMap struct {
	m map[reflect.Type]*ModelStruct
	sync.RWMutex
}

//var modelStructs = map[reflect.Type]*ModelStruct{}
var modelStructs = &modelStructMap{
	m: map[reflect.Type]*ModelStruct{},
}

var DefaultTableNameHandler = func(db *DB, defaultTableName string) string {
	return defaultTableName
}

type ModelStruct struct {
	PrimaryFields    []*StructField
	StructFields     []*StructField
	ModelType        reflect.Type
	defaultTableName string
	partial          sync.WaitGroup
	full             sync.WaitGroup
}

func (s ModelStruct) TableName(db *DB) string {
	return DefaultTableNameHandler(db, s.defaultTableName)
}

type StructField struct {
	DBName          string
	Name            string
	Names           []string
	IsPrimaryKey    bool
	IsNormal        bool
	IsIgnored       bool
	IsScanner       bool
	HasDefaultValue bool
	Tag             reflect.StructTag
	Struct          reflect.StructField
	IsForeignKey    bool
	Relationship    *Relationship
}

func (structField *StructField) clone() *StructField {
	return &StructField{
		DBName:          structField.DBName,
		Name:            structField.Name,
		Names:           structField.Names,
		IsPrimaryKey:    structField.IsPrimaryKey,
		IsNormal:        structField.IsNormal,
		IsIgnored:       structField.IsIgnored,
		IsScanner:       structField.IsScanner,
		HasDefaultValue: structField.HasDefaultValue,
		Tag:             structField.Tag,
		Struct:          structField.Struct,
		IsForeignKey:    structField.IsForeignKey,
		Relationship:    structField.Relationship,
	}
}

type Relationship struct {
	Kind                               string
	PolymorphicType                    string
	PolymorphicDBName                  string
	ForeignFieldNames                  []string
	ForeignDBNames                     []string
	AssociationForeignFieldNames       []string
	AssociationForeignStructFieldNames []string
	AssociationForeignDBNames          []string
	JoinTableHandler                   JoinTableHandlerInterface
}

func (scope *Scope) GetModelStruct() *ModelStruct {
	return scope.getModelStructInternal(true)
}

func (scope *Scope) getModelStructInternal(full bool) *ModelStruct {
	var modelStruct ModelStruct

	reflectValue := reflect.Indirect(reflect.ValueOf(scope.Value))
	if !reflectValue.IsValid() {
		return &modelStruct
	}

	if reflectValue.Kind() == reflect.Slice {
		reflectValue = reflect.Indirect(reflect.New(reflectValue.Type().Elem()))
	}

	scopeType := reflectValue.Type()

	if scopeType.Kind() == reflect.Ptr {
		scopeType = scopeType.Elem()
	}

	modelStructs.RLock()
	if value, ok := modelStructs.m[scopeType]; ok {
		modelStructs.RUnlock()
		// in case the modelstruct is not yet ready, wait
		if full {
			value.full.Wait()
		} else {
			value.partial.Wait()
		}
		return value
	}
	modelStructs.RUnlock()
	// upgrade lock. Note: another goroutine might get here before us.
	modelStructs.Lock()
	if value, ok := modelStructs.m[scopeType]; ok {
		// someone got here faster, unlock, but wait for the ModelStruct to be completed
		modelStructs.Unlock()
		// wait for modelstruct to be completed
		if full {
			value.full.Wait()
		} else {
			value.partial.Wait()
		}
		return value
	}
	// we have an excludive lock on the map, insert a modelstruct now, but add to its waitgroups,
	// then unlock the map so other structs can be inserted
	modelStruct.partial.Add(1)
	modelStruct.full.Add(1)
	defer modelStruct.full.Done()
	modelStructs.m[scopeType] = &modelStruct
	modelStructs.Unlock()

	modelStruct.ModelType = scopeType
	if scopeType.Kind() != reflect.Struct {
		modelStruct.partial.Done()
		return &modelStruct
	}

	// Set tablename
	type tabler interface {
		TableName() string
	}

	if tabler, ok := reflect.New(scopeType).Interface().(interface {
		TableName() string
	}); ok {
		modelStruct.defaultTableName = tabler.TableName()
	} else {
		name := ToDBName(scopeType.Name())
		if scope.db == nil || !scope.db.parent.singularTable {
			name = inflection.Plural(name)
		}

		modelStruct.defaultTableName = name
	}

	// Get all fields
	fields := []*StructField{}
	for i := 0; i < scopeType.NumField(); i++ {
		if fieldStruct := scopeType.Field(i); ast.IsExported(fieldStruct.Name) {
			field := &StructField{
				Struct: fieldStruct,
				Name:   fieldStruct.Name,
				Names:  []string{fieldStruct.Name},
				Tag:    fieldStruct.Tag,
			}

			if fieldStruct.Tag.Get("sql") == "-" {
				field.IsIgnored = true
			} else {
				sqlSettings := parseTagSetting(field.Tag.Get("sql"))
				gormSettings := parseTagSetting(field.Tag.Get("gorm"))
				if _, ok := gormSettings["PRIMARY_KEY"]; ok {
					field.IsPrimaryKey = true
					modelStruct.PrimaryFields = append(modelStruct.PrimaryFields, field)
				}

				if _, ok := sqlSettings["DEFAULT"]; ok {
					field.HasDefaultValue = true
				}

				if value, ok := gormSettings["COLUMN"]; ok {
					field.DBName = value
				} else {
					field.DBName = ToDBName(fieldStruct.Name)
				}
			}
			fields = append(fields, field)
		}
	}

	var finished = make(chan bool)
	go func(finished chan bool) {
		var firstPass, secondPass sync.WaitGroup
		// this mutex makes sure deferred second pass goroutines do not run concurrently.
		var secondPassMx sync.Mutex
		firstPass.Add(1)
		for _, field := range fields {
			if !field.IsIgnored {
				fieldStruct := field.Struct
				indirectType := fieldStruct.Type
				if indirectType.Kind() == reflect.Ptr {
					indirectType = indirectType.Elem()
				}

				if _, isScanner := reflect.New(indirectType).Interface().(sql.Scanner); isScanner {
					field.IsScanner, field.IsNormal = true, true
				}

				if _, isTime := reflect.New(indirectType).Interface().(*time.Time); isTime {
					field.IsNormal = true
				}

				if !field.IsNormal {
					gormSettings := parseTagSetting(field.Tag.Get("gorm"))
					toScope := scope.New(reflect.New(fieldStruct.Type).Interface())

					getForeignField := func(column string, fields []*StructField) *StructField {
						for _, field := range fields {
							if field.Name == column || field.DBName == ToDBName(column) {
								return field
							}
						}
						return nil
					}

					var relationship = &Relationship{}

					if polymorphic := gormSettings["POLYMORPHIC"]; polymorphic != "" {
						toModelStruct := toScope.getModelStructInternal(false)
						if polymorphicField := getForeignField(polymorphic+"Id", toModelStruct.StructFields); polymorphicField != nil {
							if polymorphicType := getForeignField(polymorphic+"Type", toModelStruct.StructFields); polymorphicType != nil {
								relationship.ForeignFieldNames = []string{polymorphicField.Name}
								relationship.ForeignDBNames = []string{polymorphicField.DBName}
								relationship.AssociationForeignFieldNames = []string{scope.primaryFieldInternal(&modelStruct).Name}
								relationship.AssociationForeignDBNames = []string{scope.primaryFieldInternal(&modelStruct).DBName}
								relationship.PolymorphicType = polymorphicType.Name
								relationship.PolymorphicDBName = polymorphicType.DBName
								polymorphicType.IsForeignKey = true
								polymorphicField.IsForeignKey = true
							}
						}
					}

					var foreignKeys []string
					if foreignKey, ok := gormSettings["FOREIGNKEY"]; ok {
						foreignKeys = append(foreignKeys, foreignKey)
					}
					switch indirectType.Kind() {
					case reflect.Slice:
						elemType := indirectType.Elem()
						if elemType.Kind() == reflect.Ptr {
							elemType = elemType.Elem()
						}

						if elemType.Kind() == reflect.Struct { // recursive calls in this branch, defer it
							secondPass.Add(1)
							field := field // close on current value of field
							go func() {
								secondPassMx.Lock()
								defer secondPassMx.Unlock()
								firstPass.Wait()
								if many2many := gormSettings["MANY2MANY"]; many2many != "" {
									relationship.Kind = "many_to_many"

									// foreign keys
									if len(foreignKeys) == 0 {
										for _, field := range scope.primaryFieldsInternal(&modelStruct) {
											foreignKeys = append(foreignKeys, field.DBName)
										}
									}

									for _, foreignKey := range foreignKeys {
										if field, ok := scope.fieldByNameInternal(foreignKey, &modelStruct); ok {
											relationship.ForeignFieldNames = append(relationship.ForeignFieldNames, field.DBName)
											joinTableDBName := ToDBName(scopeType.Name()) + "_" + field.DBName
											relationship.ForeignDBNames = append(relationship.ForeignDBNames, joinTableDBName)
										}
									}

									toModelStruct := toScope.getModelStructInternal(false)
									// association foreign keys
									var associationForeignKeys []string
									if foreignKey := gormSettings["ASSOCIATIONFOREIGNKEY"]; foreignKey != "" {
										associationForeignKeys = []string{gormSettings["ASSOCIATIONFOREIGNKEY"]}
									} else {
										for _, field := range toScope.primaryFieldsInternal(toModelStruct) {
											associationForeignKeys = append(associationForeignKeys, field.DBName)
										}
									}

									for _, name := range associationForeignKeys {
										if field, ok := toScope.fieldByNameInternal(name, toModelStruct); ok {
											relationship.AssociationForeignFieldNames = append(relationship.AssociationForeignFieldNames, field.DBName)
											relationship.AssociationForeignStructFieldNames = append(relationship.AssociationForeignFieldNames, field.Name)
											joinTableDBName := ToDBName(elemType.Name()) + "_" + field.DBName
											relationship.AssociationForeignDBNames = append(relationship.AssociationForeignDBNames, joinTableDBName)
										}
									}

									joinTableHandler := JoinTableHandler{}
									joinTableHandler.Setup(relationship, many2many, scopeType, elemType)
									relationship.JoinTableHandler = &joinTableHandler
									field.Relationship = relationship
								} else {
									relationship.Kind = "has_many"

									toModelStruct := toScope.getModelStructInternal(false)
									if len(foreignKeys) == 0 {
										for _, field := range scope.primaryFieldsInternal(&modelStruct) {
											if foreignField := getForeignField(scopeType.Name()+field.Name, toModelStruct.StructFields); foreignField != nil {
												relationship.AssociationForeignFieldNames = append(relationship.AssociationForeignFieldNames, field.Name)
												relationship.AssociationForeignDBNames = append(relationship.AssociationForeignDBNames, field.DBName)
												relationship.ForeignFieldNames = append(relationship.ForeignFieldNames, foreignField.Name)
												relationship.ForeignDBNames = append(relationship.ForeignDBNames, foreignField.DBName)
												foreignField.IsForeignKey = true
											}
										}
									} else {
										for _, foreignKey := range foreignKeys {
											if foreignField := getForeignField(foreignKey, toModelStruct.StructFields); foreignField != nil {
												relationship.AssociationForeignFieldNames = append(relationship.AssociationForeignFieldNames, scope.primaryFieldInternal(&modelStruct).Name)
												relationship.AssociationForeignDBNames = append(relationship.AssociationForeignDBNames, scope.primaryFieldInternal(&modelStruct).DBName)
												relationship.ForeignFieldNames = append(relationship.ForeignFieldNames, foreignField.Name)
												relationship.ForeignDBNames = append(relationship.ForeignDBNames, foreignField.DBName)
												foreignField.IsForeignKey = true
											}
										}
									}

									if len(relationship.ForeignFieldNames) != 0 {
										field.Relationship = relationship
									}
								}
								modelStruct.StructFields = append(modelStruct.StructFields, field)
								secondPass.Done()
							}()
							continue
						} else {
							field.IsNormal = true
						}
					case reflect.Struct:
						if _, ok := gormSettings["EMBEDDED"]; ok || fieldStruct.Anonymous {
							toModelStruct := toScope.getModelStructInternal(false)
							for _, toField := range toModelStruct.StructFields {
								toField = toField.clone()
								toField.Names = append([]string{fieldStruct.Name}, toField.Names...)
								modelStruct.StructFields = append(modelStruct.StructFields, toField)
								if toField.IsPrimaryKey {
									modelStruct.PrimaryFields = append(modelStruct.PrimaryFields, toField)
								}
							}
							continue
						} else { // recursive calls in this branch, defer it
							secondPass.Add(1)
							field := field // close on current value of field
							go func() {
								secondPassMx.Lock()
								defer secondPassMx.Unlock()
								firstPass.Wait()
								toModelStruct := toScope.getModelStructInternal(false)
								if len(foreignKeys) == 0 {
									for _, f := range scope.primaryFieldsInternal(&modelStruct) {
										if foreignField := getForeignField(modelStruct.ModelType.Name()+f.Name, toModelStruct.StructFields); foreignField != nil {
											relationship.AssociationForeignFieldNames = append(relationship.AssociationForeignFieldNames, f.Name)
											relationship.AssociationForeignDBNames = append(relationship.AssociationForeignDBNames, f.DBName)
											relationship.ForeignFieldNames = append(relationship.ForeignFieldNames, foreignField.Name)
											relationship.ForeignDBNames = append(relationship.ForeignDBNames, foreignField.DBName)
											foreignField.IsForeignKey = true
										}
									}
								} else {
									for _, foreignKey := range foreignKeys {
										if foreignField := getForeignField(foreignKey, toModelStruct.StructFields); foreignField != nil {
											relationship.AssociationForeignFieldNames = append(relationship.AssociationForeignFieldNames, scope.primaryFieldInternal(&modelStruct).Name)
											relationship.AssociationForeignDBNames = append(relationship.AssociationForeignDBNames, scope.primaryFieldInternal(&modelStruct).DBName)
											relationship.ForeignFieldNames = append(relationship.ForeignFieldNames, foreignField.Name)
											relationship.ForeignDBNames = append(relationship.ForeignDBNames, foreignField.DBName)
											foreignField.IsForeignKey = true
										}
									}
								}

								if len(relationship.ForeignFieldNames) != 0 {
									relationship.Kind = "has_one"
									field.Relationship = relationship
								} else {
									if len(foreignKeys) == 0 {
										for _, f := range toScope.primaryFieldsInternal(toModelStruct) {
											if foreignField := getForeignField(field.Name+f.Name, fields); foreignField != nil {
												relationship.AssociationForeignFieldNames = append(relationship.AssociationForeignFieldNames, f.Name)
												relationship.AssociationForeignDBNames = append(relationship.AssociationForeignDBNames, f.DBName)
												relationship.ForeignFieldNames = append(relationship.ForeignFieldNames, foreignField.Name)
												relationship.ForeignDBNames = append(relationship.ForeignDBNames, foreignField.DBName)
												foreignField.IsForeignKey = true
											}
										}
									} else {
										for _, foreignKey := range foreignKeys {
											if foreignField := getForeignField(foreignKey, fields); foreignField != nil {
												relationship.AssociationForeignFieldNames = append(relationship.AssociationForeignFieldNames, toScope.primaryFieldInternal(toModelStruct).Name)
												relationship.AssociationForeignDBNames = append(relationship.AssociationForeignDBNames, toScope.primaryFieldInternal(toModelStruct).DBName)
												relationship.ForeignFieldNames = append(relationship.ForeignFieldNames, foreignField.Name)
												relationship.ForeignDBNames = append(relationship.ForeignDBNames, foreignField.DBName)
												foreignField.IsForeignKey = true
											}
										}
									}

									if len(relationship.ForeignFieldNames) != 0 {
										relationship.Kind = "belongs_to"
										field.Relationship = relationship
									}
								}
								modelStruct.StructFields = append(modelStruct.StructFields, field)
								secondPass.Done()
							}()
							continue
						}
					default:
						field.IsNormal = true
					}
				}

				if field.IsNormal {
					if len(modelStruct.PrimaryFields) == 0 && field.DBName == "id" {
						field.IsPrimaryKey = true
						modelStruct.PrimaryFields = append(modelStruct.PrimaryFields, field)
					}
				}
			}
			modelStruct.StructFields = append(modelStruct.StructFields, field)
		}
		// second pass
		modelStruct.partial.Done()
		firstPass.Done()
		secondPass.Wait()
		finished <- true
	}(finished)

	//modelStructs.m[scopeType] = &modelStruct

	<-finished

	return &modelStruct
}

func (scope *Scope) GetStructFields() (fields []*StructField) {
	return scope.GetModelStruct().StructFields
}

func (scope *Scope) generateSqlTag(field *StructField) string {
	var sqlType string
	structType := field.Struct.Type
	if structType.Kind() == reflect.Ptr {
		structType = structType.Elem()
	}
	reflectValue := reflect.Indirect(reflect.New(structType))
	sqlSettings := parseTagSetting(field.Tag.Get("sql"))

	if value, ok := sqlSettings["TYPE"]; ok {
		sqlType = value
	}

	additionalType := sqlSettings["NOT NULL"] + " " + sqlSettings["UNIQUE"]
	if value, ok := sqlSettings["DEFAULT"]; ok {
		additionalType = additionalType + " DEFAULT " + value
	}

	if field.IsScanner {
		var getScannerValue func(reflect.Value)
		getScannerValue = func(value reflect.Value) {
			reflectValue = value
			if _, isScanner := reflect.New(reflectValue.Type()).Interface().(sql.Scanner); isScanner && reflectValue.Kind() == reflect.Struct {
				getScannerValue(reflectValue.Field(0))
			}
		}
		getScannerValue(reflectValue)
	}

	if sqlType == "" {
		var size = 255

		if value, ok := sqlSettings["SIZE"]; ok {
			size, _ = strconv.Atoi(value)
		}

		_, autoIncrease := sqlSettings["AUTO_INCREMENT"]
		if field.IsPrimaryKey {
			autoIncrease = true
		}

		sqlType = scope.Dialect().SqlTag(reflectValue, size, autoIncrease)
	}

	if strings.TrimSpace(additionalType) == "" {
		return sqlType
	} else {
		return fmt.Sprintf("%v %v", sqlType, additionalType)
	}
}

func parseTagSetting(str string) map[string]string {
	tags := strings.Split(str, ";")
	setting := map[string]string{}
	for _, value := range tags {
		v := strings.Split(value, ":")
		k := strings.TrimSpace(strings.ToUpper(v[0]))
		if len(v) == 2 {
			setting[k] = v[1]
		} else {
			setting[k] = k
		}
	}
	return setting
}
