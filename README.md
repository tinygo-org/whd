# whd - Wifi Host Driver

Wifi host driver for driving cypress devices.

# ⚠️ Repo currently in design phase ⚠️
We are evaluating how we'll design the interface for Ethernet devices. 

## Package structure
- whd - Root directory with shared WHD constructs

- dev - Directory with specific device drivers
    - cyw43439 - CYW43439 device driver used on the Raspberry Pi Pico W series of boards.

- dev/x/firmware - Directory with firmware specific to the parent directory device.

## Examples
See examples to run on Raspberry Pi Pico [`dev/cyw43439`](dev/cyw43439/examples/).

```
echo "RouterSSID" > dev/examples/cyw43439/examples
tinygo build -target=pico -scheduler=tasks -size=full ./dev/cyw43439/examples/dhcp 
```

