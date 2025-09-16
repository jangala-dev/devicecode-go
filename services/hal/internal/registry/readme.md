# Device Registry

Maps device `type` strings (from config) to builder implementations.

## Concepts

- `Builder` interface: `Build(BuildInput) (BuildOutput, error)`
- `BuildInput` contains factories (I²C, GPIO), device id/type, params JSON, and bus reference.
- `BuildOutput` returns an `Adaptor`, optional `BusID` (for shared worker), `SampleEvery` (period), and optional GPIO `IRQ` request.

## Usage

Device packages register in `init()`:

```go
func init() {
  registry.RegisterBuilder("aht20", aht20Builder{})
}
````

Builder implementations should:

* Validate bus references.
* Decode `params`.
* Construct the platform device via factories.
* Return `SampleEvery` for periodic producers; zero for event-only devices.
