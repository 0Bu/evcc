package vehicle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/charger"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/util/templates"
	"github.com/evcc-io/evcc/vehicle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func teslaBLEConfig(t *testing.T, server *httptest.Server) map[string]any {
	t.Helper()
	host, port, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	return map[string]any{"template": "tesla-ble", "vin": "test-vin", "url": "http://" + host, "port": port}
}

func TestTeslaBLECommandTimeouts(t *testing.T) {
	for _, timeout := range []string{"", "45s"} {
		t.Run("render/"+timeout, func(t *testing.T) {
			values := map[string]any{"template": "tesla-ble", "vin": "test-vin", "url": "http://127.0.0.1"}
			expected := "30s"
			if timeout != "" {
				values["timeout"] = timeout
				expected = timeout
			}
			instance, err := templates.RenderInstance(templates.Vehicle, values)
			require.NoError(t, err)
			for _, name := range []string{"chargeEnable", "maxcurrent", "wakeup"} {
				settings, ok := instance.Other[name].(map[string]any)
				require.True(t, ok, "missing HTTP plugin %s", name)
				require.Equal(t, expected, settings["timeout"], name)
			}
		})
	}
	for _, command := range []string{"charge_stop", "set_charging_amps", "wake_up"} {
		t.Run("http/"+command, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.True(t, strings.HasSuffix(r.URL.Path, "/command/"+command))
				requests.Add(1)
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
					_, _ = w.Write([]byte(`{"response":{"result":true}}`))
				}
			}))
			defer server.Close()
			values := teslaBLEConfig(t, server)
			values["timeout"] = "500ms"
			v, err := vehicle.NewFromConfig(t.Context(), "template", values)
			require.NoError(t, err)
			switch command {
			case "charge_stop":
				setter, ok := api.Cap[api.ChargeController](v)
				require.True(t, ok)
				err = setter.ChargeEnable(false)
			case "set_charging_amps":
				setter, ok := api.Cap[api.CurrentController](v)
				require.True(t, ok)
				err = setter.MaxCurrent(16)
			case "wake_up":
				setter, ok := api.Cap[api.Resurrector](v)
				require.True(t, ok)
				err = setter.WakeUp()
			}
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.EqualValues(t, 1, requests.Load())
		})
	}
}

type teslaBLELoadpoint struct {
	loadpoint.API
	vehicle api.Vehicle
}

func (lp *teslaBLELoadpoint) GetVehicle() api.Vehicle { return lp.vehicle }

func TestTeslaBLEStopWhileStarting(t *testing.T) {
	for _, tc := range []struct {
		state  string
		status api.ChargeStatus
		stop   bool
	}{
		{"Starting", api.StatusB, true},
		{"Charging", api.StatusC, true},
		{"Stopped", api.StatusB, true},
		{"NoPower", api.StatusB, true},
		{"Complete", api.StatusB, true},
		{"Disconnected", api.StatusA, false},
		{"Unknown", api.StatusA, false},
	} {
		t.Run(tc.state, func(t *testing.T) {
			var stops atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/vehicle_data") {
					_, _ = fmt.Fprintf(w, `{"response":{"response":{"charge_state":{"charging_state":%q}}}}`, tc.state)
					return
				}
				assert.Equal(t, http.MethodPost, r.Method)
				assert.True(t, strings.HasSuffix(r.URL.Path, "/command/charge_stop"))
				var body any
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				assert.Equal(t, false, body)
				stops.Add(1)
				_, _ = w.Write([]byte(`{"response":{"result":true}}`))
			}))
			defer server.Close()
			v, err := vehicle.NewFromConfig(t.Context(), "template", teslaBLEConfig(t, server))
			require.NoError(t, err)
			c, err := charger.NewVehicleApiFromConfig(nil)
			require.NoError(t, err)
			c.(*charger.VehicleApi).LoadpointControl(&teslaBLELoadpoint{vehicle: v})
			status, err := c.Status()
			require.NoError(t, err)
			assert.Equal(t, tc.status, status)
			require.NoError(t, c.Enable(false))
			expected := int32(0)
			if tc.stop {
				expected = 1
			}
			require.Equal(t, expected, stops.Load())
		})
	}
}
