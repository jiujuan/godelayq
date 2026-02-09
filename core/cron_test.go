package core

import (
	"testing"
	"time"
)

func TestNewCronParser(t *testing.T) {
	parser := NewCronParser()
	// Test that parser can be created without panic
	// and can parse a simple cron expression
	_, err := parser.Next("* * * * *", time.Now())
	if err != nil {
		t.Errorf("Expected parser to work, got error: %v", err)
	}
}

func TestCronParser_Next(t *testing.T) {
	parser := NewCronParser()
	
	tests := []struct {
		name     string
		cronExpr string
		now      time.Time
		wantErr  bool
		validate func(t *testing.T, next time.Time, now time.Time)
	}{
		{
			name:     "Every minute",
			cronExpr: "* * * * *",
			now:      time.Date(2024, 1, 1, 12, 30, 0, 0, time.UTC),
			wantErr:  false,
			validate: func(t *testing.T, next time.Time, now time.Time) {
				expected := time.Date(2024, 1, 1, 12, 31, 0, 0, time.UTC)
				if !next.Equal(expected) {
					t.Errorf("Expected next time %v, got %v", expected, next)
				}
			},
		},
		{
			name:     "Every hour at minute 0",
			cronExpr: "0 * * * *",
			now:      time.Date(2024, 1, 1, 12, 30, 0, 0, time.UTC),
			wantErr:  false,
			validate: func(t *testing.T, next time.Time, now time.Time) {
				expected := time.Date(2024, 1, 1, 13, 0, 0, 0, time.UTC)
				if !next.Equal(expected) {
					t.Errorf("Expected next time %v, got %v", expected, next)
				}
			},
		},
		{
			name:     "Every day at midnight",
			cronExpr: "0 0 * * *",
			now:      time.Date(2024, 1, 1, 12, 30, 0, 0, time.UTC),
			wantErr:  false,
			validate: func(t *testing.T, next time.Time, now time.Time) {
				expected := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
				if !next.Equal(expected) {
					t.Errorf("Expected next time %v, got %v", expected, next)
				}
			},
		},
		{
			name:     "Every Monday at 9:00 AM",
			cronExpr: "0 9 * * 1",
			now:      time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC), // Monday
			wantErr:  false,
			validate: func(t *testing.T, next time.Time, now time.Time) {
				// Next Monday at 9:00 AM
				if next.Weekday() != time.Monday {
					t.Errorf("Expected Monday, got %v", next.Weekday())
				}
				if next.Hour() != 9 || next.Minute() != 0 {
					t.Errorf("Expected 9:00 AM, got %02d:%02d", next.Hour(), next.Minute())
				}
			},
		},
		{
			name:     "First day of month at noon",
			cronExpr: "0 12 1 * *",
			now:      time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC),
			wantErr:  false,
			validate: func(t *testing.T, next time.Time, now time.Time) {
				expected := time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC)
				if !next.Equal(expected) {
					t.Errorf("Expected next time %v, got %v", expected, next)
				}
			},
		},
		{
			name:     "Invalid cron expression - too many fields",
			cronExpr: "* * * * * *",
			now:      time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
			wantErr:  true,
		},
		{
			name:     "Invalid cron expression - empty",
			cronExpr: "",
			now:      time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
			wantErr:  true,
		},
		{
			name:     "Invalid cron expression - bad syntax",
			cronExpr: "invalid cron",
			now:      time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
			wantErr:  true,
		},
		{
			name:     "Every 5 minutes",
			cronExpr: "*/5 * * * *",
			now:      time.Date(2024, 1, 1, 12, 32, 0, 0, time.UTC),
			wantErr:  false,
			validate: func(t *testing.T, next time.Time, now time.Time) {
				expected := time.Date(2024, 1, 1, 12, 35, 0, 0, time.UTC)
				if !next.Equal(expected) {
					t.Errorf("Expected next time %v, got %v", expected, next)
				}
			},
		},
		{
			name:     "Specific time - 3:30 PM every day",
			cronExpr: "30 15 * * *",
			now:      time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC),
			wantErr:  false,
			validate: func(t *testing.T, next time.Time, now time.Time) {
				expected := time.Date(2024, 1, 1, 15, 30, 0, 0, time.UTC)
				if !next.Equal(expected) {
					t.Errorf("Expected next time %v, got %v", expected, next)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next, err := parser.Next(tt.cronExpr, tt.now)
			
			if tt.wantErr {
				if err == nil {
					t.Errorf("Expected error for cron expression %q, got nil", tt.cronExpr)
				}
				return
			}
			
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}
			
			// Verify next time is after now
			if !next.After(tt.now) {
				t.Errorf("Expected next time %v to be after now %v", next, tt.now)
			}
			
			// Run custom validation if provided
			if tt.validate != nil {
				tt.validate(t, next, tt.now)
			}
		})
	}
}

func TestCronParser_Next_EdgeCases(t *testing.T) {
	parser := NewCronParser()
	
	t.Run("Leap year - Feb 29", func(t *testing.T) {
		// 2024 is a leap year
		cronExpr := "0 12 29 2 *"
		now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		
		next, err := parser.Next(cronExpr, now)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		
		expected := time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC)
		if !next.Equal(expected) {
			t.Errorf("Expected %v, got %v", expected, next)
		}
	})
	
	t.Run("Year boundary", func(t *testing.T) {
		cronExpr := "0 0 1 1 *" // Jan 1 at midnight
		now := time.Date(2024, 12, 31, 23, 0, 0, 0, time.UTC)
		
		next, err := parser.Next(cronExpr, now)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		
		expected := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		if !next.Equal(expected) {
			t.Errorf("Expected %v, got %v", expected, next)
		}
	})
	
	t.Run("Multiple calls return sequential times", func(t *testing.T) {
		cronExpr := "*/10 * * * *" // Every 10 minutes
		now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
		
		next1, err := parser.Next(cronExpr, now)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		
		next2, err := parser.Next(cronExpr, next1)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		
		if !next2.After(next1) {
			t.Errorf("Expected next2 %v to be after next1 %v", next2, next1)
		}
		
		diff := next2.Sub(next1)
		if diff != 10*time.Minute {
			t.Errorf("Expected 10 minute difference, got %v", diff)
		}
	})
}
