# HAL Config

Defines the configuration schema supplied to the HAL service on the `config/hal` bus topic.

## Types

- `HALConfig`  
  ```json
  { "devices": [Device, ...] }

* `Device`

  * `id` (string): stable device identifier (unique within the HAL instance).
  * `type` (string): device type key resolved via the registry (e.g. `aht20`, `ltc4015`, `gpio`).
  * `params` (any, optional): device-specific configuration (JSON).
  * `bus_ref` (object, optional): identifies a shared bus instance.

    * `type` (string): e.g. `i2c`, `spi`.
    * `id` (string): platform bus identifier, e.g. `i2c0`.

## Usage

Publish an object matching `HALConfig` on `config/hal`. The service is idempotent:

* New devices are built and started.
* Unchanged devices are left in place.
* Missing devices are torn down (state set to `down`, retained info cleared).

## Example

```json
{
  "devices": [
    { "id": "env0", "type": "aht20", "bus_ref": {"type":"i2c","id":"i2c0"}, "params": {"addr": 56} },
    { "id": "chg0", "type": "ltc4015", "bus_ref": {"type":"i2c","id":"i2c1"}, "params": {"cells":4, "chem":"lead_acid"} },
    { "id": "gp17", "type": "gpio", "params": {"pin":17, "mode":"input", "pull":"up", "irq":{"edge":"falling","debounce_ms":10}} }
  ]
}
```
