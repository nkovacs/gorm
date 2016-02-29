package gorm_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func runMigration() {
	if err := DB.DropTableIfExists(&User{}).Error; err != nil {
		fmt.Printf("Got error when try to delete table users, %+v\n", err)
	}

	for _, table := range []string{"animals", "user_languages"} {
		DB.Exec(fmt.Sprintf("drop table %v;", table))
	}

	values := []interface{}{&Product{}, &Email{}, &Address{}, &CreditCard{}, &Company{}, &Role{}, &Language{}, &HNPost{}, &EngadgetPost{}, &Animal{}, &User{}, &JoinTable{}, &Post{}, &Category{}, &Comment{}, &Cat{}, &Dog{}, &Toy{}}
	for _, value := range values {
		DB.DropTable(value)
	}

	if err := DB.AutoMigrate(values...).Error; err != nil {
		panic(fmt.Sprintf("No error should happen when create table, but got %+v", err))
	}
}

func TestIndexes(t *testing.T) {
	if err := DB.Model(&Email{}).AddIndex("idx_email_email", "email").Error; err != nil {
		t.Errorf("Got error when tried to create index: %+v", err)
	}

	scope := DB.NewScope(&Email{})
	if !scope.Dialect().HasIndex(scope, scope.TableName(), "idx_email_email") {
		t.Errorf("Email should have index idx_email_email")
	}

	if err := DB.Model(&Email{}).RemoveIndex("idx_email_email").Error; err != nil {
		t.Errorf("Got error when tried to remove index: %+v", err)
	}

	if scope.Dialect().HasIndex(scope, scope.TableName(), "idx_email_email") {
		t.Errorf("Email's index idx_email_email should be deleted")
	}

	if err := DB.Model(&Email{}).AddIndex("idx_email_email_and_user_id", "user_id", "email").Error; err != nil {
		t.Errorf("Got error when tried to create index: %+v", err)
	}

	if !scope.Dialect().HasIndex(scope, scope.TableName(), "idx_email_email_and_user_id") {
		t.Errorf("Email should have index idx_email_email_and_user_id")
	}

	if err := DB.Model(&Email{}).RemoveIndex("idx_email_email_and_user_id").Error; err != nil {
		t.Errorf("Got error when tried to remove index: %+v", err)
	}

	if scope.Dialect().HasIndex(scope, scope.TableName(), "idx_email_email_and_user_id") {
		t.Errorf("Email's index idx_email_email_and_user_id should be deleted")
	}

	if err := DB.Model(&Email{}).AddUniqueIndex("idx_email_email_and_user_id", "user_id", "email").Error; err != nil {
		t.Errorf("Got error when tried to create index: %+v", err)
	}

	if !scope.Dialect().HasIndex(scope, scope.TableName(), "idx_email_email_and_user_id") {
		t.Errorf("Email should have index idx_email_email_and_user_id")
	}

	if DB.Save(&User{Name: "unique_indexes", Emails: []Email{{Email: "user1@example.comiii"}, {Email: "user1@example.com"}, {Email: "user1@example.com"}}}).Error == nil {
		t.Errorf("Should get to create duplicate record when having unique index")
	}

	var user = User{Name: "sample_user"}
	DB.Save(&user)
	if DB.Model(&user).Association("Emails").Append(Email{Email: "not-1duplicated@gmail.com"}, Email{Email: "not-duplicated2@gmail.com"}).Error != nil {
		t.Errorf("Should get no error when append two emails for user")
	}

	if DB.Model(&user).Association("Emails").Append(Email{Email: "duplicated@gmail.com"}, Email{Email: "duplicated@gmail.com"}).Error == nil {
		t.Errorf("Should get no duplicated email error when insert duplicated emails for a user")
	}

	if err := DB.Model(&Email{}).RemoveIndex("idx_email_email_and_user_id").Error; err != nil {
		t.Errorf("Got error when tried to remove index: %+v", err)
	}

	if scope.Dialect().HasIndex(scope, scope.TableName(), "idx_email_email_and_user_id") {
		t.Errorf("Email's index idx_email_email_and_user_id should be deleted")
	}

	if DB.Save(&User{Name: "unique_indexes", Emails: []Email{{Email: "user1@example.com"}, {Email: "user1@example.com"}}}).Error != nil {
		t.Errorf("Should be able to create duplicated emails after remove unique index")
	}
}

type MyTime time.Time

type BigEmail struct {
	Id           int64
	UserId       int64
	Email        string    `sql:"index:idx_email_agent"`
	UserAgent    string    `sql:"index:idx_email_agent"`
	RegisteredAt time.Time `sql:"unique_index"`
	//CustomTime   MyTime          `sql:"type:timestamp"`
	CustomData json.RawMessage `sql:"type:longblob"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type BigEmail2 struct {
	Id           int64
	UserId       int64
	Email        string    `sql:"index:idx_email_agent"`
	UserAgent    string    `sql:"index:idx_email_agent"`
	RegisteredAt time.Time `sql:"unique_index"`
	CustomTime   time.Time
	CustomData   []byte
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (b BigEmail) TableName() string {
	return "emails"
}

func (b BigEmail2) TableName() string {
	return "emails"
}

func TestAutoMigration(t *testing.T) {
	DB.AutoMigrate(&Address{})
	if err := DB.Table("emails").AutoMigrate(&BigEmail{}).Error; err != nil {
		t.Errorf("Auto Migrate should not raise any error")
	}

	customData := json.RawMessage(`{"extra1": "foo", "extra2": bar}`)

	/*
		DB.Debug().Save(&BigEmail2{
			Email:        "jinzhu@example.org",
			UserAgent:    "pc",
			RegisteredAt: time.Now(),
			CustomTime:   time.Now(),
			CustomData:   []byte(customData),
		})
	*/

	DB.Debug().Save(&BigEmail{
		Email:        "jinzhu@example.org",
		UserAgent:    "pc",
		RegisteredAt: time.Now(),
		//CustomTime:   MyTime(time.Now()),
		CustomData: customData,
	})

	scope := DB.NewScope(&BigEmail{})
	if !scope.Dialect().HasIndex(scope, scope.TableName(), "idx_email_agent") {
		t.Errorf("Failed to create index")
	}

	if !scope.Dialect().HasIndex(scope, scope.TableName(), "uix_emails_registered_at") {
		t.Errorf("Failed to create index")
	}

	var bigemail BigEmail
	DB.First(&bigemail, "user_agent = ?", "pc")
	if bigemail.Email != "jinzhu@example.org" || bigemail.UserAgent != "pc" ||
		bigemail.RegisteredAt.IsZero() || /*time.Time(bigemail.CustomTime).IsZero() ||*/
		string(bigemail.CustomData) != string(customData) {
		t.Log(bigemail)
		t.Error("Big Emails should be saved and fetched correctly")
	}
}
