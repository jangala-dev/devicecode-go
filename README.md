# devicecode-go

## Flashing ISOC Power Board via USB port on Pico
tinygo flash -stack-size=3KB -monitor -scheduler tasks -target=pico -tags "pico_bb_proto_1" main.go

## Flashing ISOC Power Board via USB port on Pico2
tinygo flash -stack-size=8KB -monitor -scheduler tasks -target=pico2 -tags "pico_bb_proto_1" main.go

-------------------

## Debugging instructions

The following instructions set you up debugging the code on an MCU, using a Pico debug module, connected to a Mac (tested on arm64).

### Environment Setup

- Install Go according to https://go.dev/doc/install
- Install TinyGo according to https://tinygo.org/getting-started/install/
- Install OpenOCD according to https://openocd.org/pages/getting-openocd.html (just use `brew install open-ocd` if on MacOs)
- Install arm-none-eabi-gdb (`brew install arm-none-eabi-gdb` on MacOs).
- Install GDB (`brew install gdb` on MacOs).
- Codesign the GDB executable - https://sourceware.org/gdb/wiki/PermissionsDarwin
- Install VSCode Extensions for Go, TinyGo & Cortex-Debug

#### Why codesign the GDB executable?

The Darwin Kernel requires the debugger to have special permissions before it is allowed to control other processes. These permissions are granted by codesigning the GDB executable.  The debugger will not work until this step is performed.

### Build and debug from VSCode

- Select target from debug dropdown (Pico or Pico2)
- Press F5 to begin debugging

![alt text](/assets/image.png)

An OpenOCD server will be started, a GDB session started.  Then the project will be built with flags specified in .vscode/tasks.json. Next, the compiled .elf file will be flashed to the MCU. The MCU will then be reset and control is handed over to you for the debug session.

You'll then be able to add breakpoints, pause, resume and reset the MCU remotely using VsCode debug tools.
