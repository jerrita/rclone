package api

import "fmt"

type ApiResponse[T any] struct {
	Code    int    `json:"code"`
	Data    T      `json:"data"`
	Message string `json:"message"`
	TraceID string `json:"x-traceID"`
}

func (e *ApiResponse[T]) Error() string {
	return fmt.Sprintf("%s (%d %s)", e.TraceID, e.Code, e.Message)
}

type GetAccessToken struct {
	ClientId     string `json:"clientID"`
	ClientSecret string `json:"clientSecret"`
}

type GetAccessTokenResponse struct {
	AccessToken string `json:"accessToken"`
	ExpiredAt   string `json:"expiredAt"`
}
