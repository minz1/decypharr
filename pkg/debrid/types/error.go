package types

type Error struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

func (e *Error) Error() string {
	return e.Message
}

var ErrDownloadLinkNotFound = &Error{
	Message: "No download link found",
	Code:    "no_download_link",
}

var ErrEmptyDownloadLink = &Error{
	Message: "Download link is empty",
	Code:    "empty_download_link",
}

var ErrInvalidDownloadLink = &Error{
	Message: "Download link is invalid",
	Code:    "invalid_download_link",
}

var ErrAvailabilityUnsupported = &Error{
	Message: "Availability checks are not supported",
	Code:    "availability_unsupported",
}
