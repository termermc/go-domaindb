package domaindb

import (
	"errors"
	"fmt"
)

// ErrNoCacheAndNoDownload is returned when there is no cached database, and downloading is disabled so there is no way to get the database.
var ErrNoCacheAndNoDownload = errors.New("no cached copy of database existed, and downloading is disabled")

// ErrDataSourceNoSource is returned when a data source has no sources.
// "No sources" means that the data source has no URLs and the Get method is nil.
var ErrDataSourceNoSource = errors.New("data source has no sources: len(Urls) == 0 or Get method is nil")

// ErrAllUrlsFailed is returned when all URLs in a data source failed.
var ErrAllUrlsFailed = errors.New("all URLs in data source failed")

// ErrDbClosed is returned when an operation is attempted on a closed database.
var ErrDbClosed = errors.New("domain database closed")

// ErrDbNameTooLong is returned when a database name exceeds DbNameMaxSize bytes.
var ErrDbNameTooLong = fmt.Errorf("database name too long, must be at most %d bytes long", DbNameMaxSize)

// NotInitializedError is returned when a function is run that required a domain database to be initialized, but it was not initialized.
// Includes the database name that was required but not initialized.
type NotInitializedError struct {
	// The name of database that was not initialized.
	Name string
}

func (err *NotInitializedError) Error() string {
	return fmt.Sprintf(`domain database "%s" not initialized`, err.Name)
}

// NewNotInitializedError creates a new NewNotInitializedError instance with the specified database name.
func NewNotInitializedError(name string) *NotInitializedError {
	return &NotInitializedError{
		Name: name,
	}
}

// NoSuchDatabaseError is returned when trying to access a domain database that does not exist.
// Includes the requested database name that did not exist.
type NoSuchDatabaseError struct {
	// The name of database that did not exist.
	Name string
}

func (err *NoSuchDatabaseError) Error() string {
	return fmt.Sprintf(`domain database "%s" does not exist`, err.Name)
}

// NewNoSuchDatabaseError creates a new NoSuchDatabaseError instance with the specified database name.
func NewNoSuchDatabaseError(name string) *NoSuchDatabaseError {
	return &NoSuchDatabaseError{
		Name: name,
	}
}
