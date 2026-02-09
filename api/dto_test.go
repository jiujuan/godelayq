package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateJobRequest_JSONMarshaling(t *testing.T) {
	triggerTime := time.Now().Add(1 * time.Hour)
	
	req := CreateJobRequest{
		Name:       "test-job",
		Delay:      "10m",
		TriggerAt:  &triggerTime,
		CronExpr:   "*/5 * * * *",
		Payload:    json.RawMessage(`{"key":"value"}`),
		IsRepeat:   true,
		MaxRetries: 3,
		RetryDelay: "30s",
	}

	// Marshal
	data, err := json.Marshal(req)
	require.NoError(t, err, "Should marshal successfully")
	assert.NotEmpty(t, data)

	// Unmarshal
	var decoded CreateJobRequest
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err, "Should unmarshal successfully")

	// Verify fields
	assert.Equal(t, req.Name, decoded.Name)
	assert.Equal(t, req.Delay, decoded.Delay)
	assert.Equal(t, req.CronExpr, decoded.CronExpr)
	assert.Equal(t, req.IsRepeat, decoded.IsRepeat)
	assert.Equal(t, req.MaxRetries, decoded.MaxRetries)
	assert.Equal(t, req.RetryDelay, decoded.RetryDelay)
}

func TestCreateJobRequest_MinimalFields(t *testing.T) {
	req := CreateJobRequest{
		Name: "minimal-job",
	}

	data, err := json.Marshal(req)
	require.NoError(t, err)

	var decoded CreateJobRequest
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, "minimal-job", decoded.Name)
	assert.Empty(t, decoded.Delay)
	assert.Nil(t, decoded.TriggerAt)
}

func TestCreateJobRequest_WithPayload(t *testing.T) {
	payload := map[string]interface{}{
		"user_id": 123,
		"action":  "send_email",
		"data": map[string]string{
			"to":      "test@example.com",
			"subject": "Test",
		},
	}

	payloadJSON, _ := json.Marshal(payload)

	req := CreateJobRequest{
		Name:    "email-job",
		Payload: json.RawMessage(payloadJSON),
	}

	data, err := json.Marshal(req)
	require.NoError(t, err)

	var decoded CreateJobRequest
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	// Verify payload
	var decodedPayload map[string]interface{}
	err = json.Unmarshal(decoded.Payload, &decodedPayload)
	require.NoError(t, err)
	assert.Equal(t, float64(123), decodedPayload["user_id"])
	assert.Equal(t, "send_email", decodedPayload["action"])
}

func TestUpdateJobRequest_JSONMarshaling(t *testing.T) {
	triggerTime := time.Now().Add(2 * time.Hour)
	maxRetries := 5

	req := UpdateJobRequest{
		TriggerAt:  &triggerTime,
		Payload:    json.RawMessage(`{"updated":"data"}`),
		MaxRetries: &maxRetries,
	}

	data, err := json.Marshal(req)
	require.NoError(t, err)

	var decoded UpdateJobRequest
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.NotNil(t, decoded.TriggerAt)
	assert.NotNil(t, decoded.MaxRetries)
	assert.Equal(t, 5, *decoded.MaxRetries)
}

func TestUpdateJobRequest_PartialUpdate(t *testing.T) {
	// Only update MaxRetries
	maxRetries := 10
	req := UpdateJobRequest{
		MaxRetries: &maxRetries,
	}

	data, err := json.Marshal(req)
	require.NoError(t, err)

	var decoded UpdateJobRequest
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Nil(t, decoded.TriggerAt)
	assert.Nil(t, decoded.Payload)
	assert.NotNil(t, decoded.MaxRetries)
	assert.Equal(t, 10, *decoded.MaxRetries)
}

func TestJobResponse_JSONMarshaling(t *testing.T) {
	now := time.Now()

	resp := JobResponse{
		ID:         "job-123",
		Name:       "test-job",
		Status:     "pending",
		TriggerAt:  now.Add(1 * time.Hour),
		Payload:    json.RawMessage(`{"test":"data"}`),
		RetryCount: 2,
		MaxRetries: 5,
		IsRepeat:   true,
		CronExpr:   "0 * * * *",
		CreatedAt:  now,
		UpdatedAt:  now,
		NextRunIn:  "1h0m0s",
	}

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	var decoded JobResponse
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, resp.ID, decoded.ID)
	assert.Equal(t, resp.Name, decoded.Name)
	assert.Equal(t, resp.Status, decoded.Status)
	assert.Equal(t, resp.RetryCount, decoded.RetryCount)
	assert.Equal(t, resp.MaxRetries, decoded.MaxRetries)
	assert.Equal(t, resp.IsRepeat, decoded.IsRepeat)
	assert.Equal(t, resp.CronExpr, decoded.CronExpr)
	assert.Equal(t, resp.NextRunIn, decoded.NextRunIn)
}

func TestJobResponse_AllStatuses(t *testing.T) {
	statuses := []string{"pending", "running", "success", "failed", "cancelled"}

	for _, status := range statuses {
		resp := JobResponse{
			ID:     "job-" + status,
			Name:   "test",
			Status: status,
		}

		data, err := json.Marshal(resp)
		require.NoError(t, err)

		var decoded JobResponse
		err = json.Unmarshal(data, &decoded)
		require.NoError(t, err)

		assert.Equal(t, status, decoded.Status)
	}
}

func TestListJobsResponse_JSONMarshaling(t *testing.T) {
	resp := ListJobsResponse{
		Total: 2,
		Items: []JobResponse{
			{
				ID:     "job-1",
				Name:   "test-1",
				Status: "pending",
			},
			{
				ID:     "job-2",
				Name:   "test-2",
				Status: "running",
			},
		},
	}

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	var decoded ListJobsResponse
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, 2, decoded.Total)
	assert.Len(t, decoded.Items, 2)
	assert.Equal(t, "job-1", decoded.Items[0].ID)
	assert.Equal(t, "job-2", decoded.Items[1].ID)
}

func TestListJobsResponse_Empty(t *testing.T) {
	resp := ListJobsResponse{
		Total: 0,
		Items: []JobResponse{},
	}

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	var decoded ListJobsResponse
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, 0, decoded.Total)
	assert.Empty(t, decoded.Items)
}

func TestStatsResponse_JSONMarshaling(t *testing.T) {
	resp := StatsResponse{
		Pending:   10,
		Running:   2,
		Completed: 50,
		Failed:    3,
		HeapSize:  12,
		Uptime:    "24h30m15s",
	}

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	var decoded StatsResponse
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, 10, decoded.Pending)
	assert.Equal(t, 2, decoded.Running)
	assert.Equal(t, 50, decoded.Completed)
	assert.Equal(t, 3, decoded.Failed)
	assert.Equal(t, 12, decoded.HeapSize)
	assert.Equal(t, "24h30m15s", decoded.Uptime)
}

func TestStatsResponse_ZeroValues(t *testing.T) {
	resp := StatsResponse{
		Pending:   0,
		Running:   0,
		Completed: 0,
		Failed:    0,
		HeapSize:  0,
		Uptime:    "0s",
	}

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	var decoded StatsResponse
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, 0, decoded.Pending)
	assert.Equal(t, 0, decoded.Running)
	assert.Equal(t, 0, decoded.Completed)
	assert.Equal(t, 0, decoded.Failed)
}

func TestErrorResponse_JSONMarshaling(t *testing.T) {
	resp := ErrorResponse{
		Code:    400,
		Message: "invalid request",
		Details: "field 'name' is required",
	}

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	var decoded ErrorResponse
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, 400, decoded.Code)
	assert.Equal(t, "invalid request", decoded.Message)
	assert.Equal(t, "field 'name' is required", decoded.Details)
}

func TestErrorResponse_WithoutDetails(t *testing.T) {
	resp := ErrorResponse{
		Code:    404,
		Message: "not found",
	}

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	var decoded ErrorResponse
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, 404, decoded.Code)
	assert.Equal(t, "not found", decoded.Message)
	assert.Empty(t, decoded.Details)
}

func TestErrorResponse_CommonHTTPCodes(t *testing.T) {
	testCases := []struct {
		code    int
		message string
	}{
		{400, "Bad Request"},
		{401, "Unauthorized"},
		{403, "Forbidden"},
		{404, "Not Found"},
		{409, "Conflict"},
		{500, "Internal Server Error"},
		{503, "Service Unavailable"},
	}

	for _, tc := range testCases {
		resp := ErrorResponse{
			Code:    tc.code,
			Message: tc.message,
		}

		data, err := json.Marshal(resp)
		require.NoError(t, err)

		var decoded ErrorResponse
		err = json.Unmarshal(data, &decoded)
		require.NoError(t, err)

		assert.Equal(t, tc.code, decoded.Code)
		assert.Equal(t, tc.message, decoded.Message)
	}
}

func TestCreateJobRequest_EmptyPayload(t *testing.T) {
	req := CreateJobRequest{
		Name:    "no-payload-job",
		Delay:   "5m",
		Payload: nil,
	}

	data, err := json.Marshal(req)
	require.NoError(t, err)

	var decoded CreateJobRequest
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, "no-payload-job", decoded.Name)
	assert.Nil(t, decoded.Payload)
}

func TestJobResponse_WithoutOptionalFields(t *testing.T) {
	now := time.Now()

	resp := JobResponse{
		ID:        "minimal-job",
		Name:      "test",
		Status:    "pending",
		TriggerAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	var decoded JobResponse
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, "minimal-job", decoded.ID)
	assert.Empty(t, decoded.CronExpr)
	assert.Empty(t, decoded.NextRunIn)
	assert.False(t, decoded.IsRepeat)
}

func TestCreateJobRequest_AllTimeFormats(t *testing.T) {
	testCases := []struct {
		name      string
		setupFunc func() CreateJobRequest
	}{
		{
			name: "With Delay",
			setupFunc: func() CreateJobRequest {
				return CreateJobRequest{
					Name:  "delay-job",
					Delay: "15m",
				}
			},
		},
		{
			name: "With TriggerAt",
			setupFunc: func() CreateJobRequest {
				triggerTime := time.Now().Add(1 * time.Hour)
				return CreateJobRequest{
					Name:      "trigger-job",
					TriggerAt: &triggerTime,
				}
			},
		},
		{
			name: "With CronExpr",
			setupFunc: func() CreateJobRequest {
				return CreateJobRequest{
					Name:     "cron-job",
					CronExpr: "0 */2 * * *",
					IsRepeat: true,
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.setupFunc()

			data, err := json.Marshal(req)
			require.NoError(t, err)

			var decoded CreateJobRequest
			err = json.Unmarshal(data, &decoded)
			require.NoError(t, err)

			assert.Equal(t, req.Name, decoded.Name)
		})
	}
}
