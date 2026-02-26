# Unified Ethernet Device Interface Design

This is AI written.

## Context

Two embedded Go network device drivers (CYW43439 WiFi, LAN8720 Ethernet PHY) both integrate with `lneto` via nearly-identical stack wrapper code (`whdstack/wstack.go` and `lannet/stack.go`). The wrappers differ only in how they call the device. A unified interface would:
- Eliminate duplicated stack wrapper code
- Enable a single `NewStack(dev, cfg)` that works with either (or future) devices
- Fill the empty `Interface` placeholder in `whd/interfaces.go`

---

# Part 1: Comprehensive External API Reference

## 1. Linux Kernel Networking

### TX Path
```c
// Driver implements:
int (*ndo_start_xmit)(struct sk_buff *skb, struct net_device *dev);
// Returns NETDEV_TX_OK or NETDEV_TX_BUSY

// Flow control:
netif_stop_queue(dev);    // pause TX when HW ring full
netif_wake_queue(dev);    // resume TX on TX-complete interrupt
```
Stack calls `ndo_start_xmit()` synchronously. Driver DMA-copies skb to HW ring. If ring full, driver returns BUSY or stops queue.

### RX Path - Legacy (pre-NAPI)
```c
// Driver ISR:
static irqreturn_t driver_isr(int irq, void *dev_id) {
    struct sk_buff *skb = alloc_skb(...);
    // DMA-copy packet into skb
    netif_rx(skb);          // queue to per-CPU backlog
    return IRQ_HANDLED;
}
// netif_rx() raises NET_RX_SOFTIRQ
// softirq handler calls netif_receive_skb() for each queued skb
```
**Pattern**: ISR allocates skb, copies data, queues via `netif_rx()`. Softirq defers actual stack processing. Problem: every packet triggers softirq scheduling.

### RX Path - NAPI (modern)
```c
// Driver registration:
netif_napi_add(dev, &napi, my_poll, 64);  // weight=64

// Driver ISR (minimal work):
static irqreturn_t my_isr(int irq, void *dev_id) {
    disable_hw_interrupts();       // prevent interrupt storm
    napi_schedule(&napi);          // schedule softirq poll
    return IRQ_HANDLED;
}

// Poll function (called from softirq context, NOT ISR):
static int my_poll(struct napi_struct *napi, int budget) {
    int work_done = 0;
    while (work_done < budget && ring_has_packet()) {
        struct sk_buff *skb = fetch_from_ring();
        napi_gro_receive(napi, skb);  // deliver to stack (with GRO coalescing)
        work_done++;
    }
    if (work_done < budget) {
        napi_complete_done(napi, work_done);  // done polling
        enable_hw_interrupts();                // re-enable for next batch
    }
    return work_done;
}
```
**ISR-to-stack sequence**:
1. **[HARDIRQ]** NIC raises interrupt
2. **[HARDIRQ]** ISR disables HW interrupts, calls `napi_schedule()` → adds to softirq poll_list
3. **[SOFTIRQ]** `net_rx_action()` iterates poll_list, calls driver's `poll(budget=64)`
4. **[SOFTIRQ]** `poll()` drains ring buffer, calls `napi_gro_receive()` per packet
5. **[SOFTIRQ]** When `poll()` returns < budget → `napi_complete_done()` re-enables HW interrupts

**Key insight**: NAPI is a **hybrid interrupt+poll model**. Interrupts trigger polling; polling disables interrupts. This prevents interrupt storms under load while maintaining low latency at low throughput. The driver's poll function processes packets in softirq context, NOT ISR context.

**Buffer ownership**: Stack allocates `sk_buff`. Driver fills via DMA. Stack owns after `napi_gro_receive()`.
**Backpressure**: `netif_stop_queue()` for TX. `netdev_max_backlog` queue for RX (default 1000).

---

## 2. Zephyr RTOS

### TX Path
```c
struct ethernet_api {
    int (*send)(const struct device *dev, struct net_pkt *pkt);
    int (*start)(const struct device *dev);
    int (*stop)(const struct device *dev);
    enum ethernet_hw_caps (*get_capabilities)(const struct device *dev);
    int (*set_config)(const struct device *dev, enum ethernet_config_type type,
                      const struct ethernet_config *config);
    int (*get_config)(const struct device *dev, enum ethernet_config_type type,
                      struct ethernet_config *config);
    const struct device *(*get_phy)(const struct device *dev);
};
// Stack calls send() synchronously. Driver DMA-copies and transmits.
```

### RX Path - ISR + Work Queue
```c
// Driver ISR (e.g., STM32 GMAC):
static void queue0_isr(const struct device *dev) {
    uint32_t isr = gmac->GMAC_ISR;  // clears interrupt
    if (isr & GMAC_ISR_RCOMP) {
        struct net_pkt *pkt = frame_get();  // assemble from DMA descriptors
        net_recv_data(get_iface(dev), pkt); // queue to iface rx_queue (k_fifo)
    }
}

// Delivery API (called by driver):
int net_recv_data(struct net_if *iface, struct net_pkt *pkt);
// Queues pkt to iface->rx_queue (k_fifo), signals network task
// Returns 0 (queued) or -1 (dropped)
```
**ISR-to-stack sequence**:
1. **[ISR]** MAC interrupt fires
2. **[ISR]** Driver reads DMA descriptors, assembles `net_pkt`
3. **[ISR]** `net_recv_data()` queues to `k_fifo`
4. **[NETWORK TASK]** Polls `k_fifo`, calls `net_if_recv_data()` → L2/L3 processing

**Buffer ownership**: Driver allocates `net_pkt` via `net_pkt_rx_alloc_with_buffer()` (ISR-safe, from pre-allocated pool). Stack owns after `net_recv_data()`.
**Backpressure**: `net_recv_data()` returns -1 if RX queue full → packet dropped.

---

## 3. lwIP

### TX Path
```c
// Driver implements low_level_output:
static err_t low_level_output(struct netif *netif, struct pbuf *p) {
    for (struct pbuf *q = p; q != NULL; q = q->next)
        send_data(q->payload, q->len);
    trigger_transmission();
    return ERR_OK;
}

// Registration:
netif->linkoutput = low_level_output;  // raw link output
netif->output = etharp_output;         // ARP-resolved output
```

### RX Path - Deferred to tcpip_thread
```c
// Driver ISR/task calls:
static void ethernetif_input(struct netif *netif) {
    struct pbuf *p = low_level_input(netif);  // read from HW
    if (p != NULL) {
        netif->input(p, netif);  // = tcpip_input()
    }
}

// tcpip_input (set during netif_add):
err_t tcpip_input(struct pbuf *p, struct netif *inp) {
    // Creates TCPIP_MSG_INPKT message
    // Queues to tcpip_thread's mailbox via sys_mbox_trypost()
    // Returns ERR_OK immediately (deferred!)
}
```
**ISR-to-stack sequence**:
1. **[ISR/TASK]** Driver calls `low_level_input()` → reads packet into `pbuf`
2. **[ISR/TASK]** Calls `netif->input()` = `tcpip_input()` → queues message
3. **[TCPIP_THREAD]** Wakes, dispatches `ethernet_input()` → ARP/IP processing

**netif struct key fields**:
```c
struct netif {
    netif_linkoutput_fn linkoutput;      // TX: raw frame output
    netif_input_fn input;                // RX: deliver to stack (= tcpip_input)
    netif_status_callback_fn link_callback;  // link state changes
    u16_t mtu;
    u8_t hwaddr[NETIF_MAX_HWADDR_LEN];
    u8_t flags;  // NETIF_FLAG_UP | NETIF_FLAG_LINK_UP | NETIF_FLAG_BROADCAST | NETIF_FLAG_ETHARP
};
```

**Buffer ownership**: Stack/driver allocates `pbuf` via `pbuf_alloc()`. Ref-counted. Stack frees after consumption.
**Backpressure**: If mailbox full, `tcpip_input()` fails → packet dropped.

---

## 4. FreeRTOS+TCP

### TX Path
```c
BaseType_t xNetworkInterfaceOutput(
    NetworkBufferDescriptor_t *pxBuffer,
    BaseType_t xReleaseAfterSend);
// Synchronous. Driver DMA-copies and transmits.
```

### RX Path - Deferred Interrupt Task
```c
// ISR (minimal):
static void ethernet_isr(void) {
    BaseType_t xWoken = pdFALSE;
    xTaskNotifyFromISR(deferred_task, RX_FLAG, eSetBits, &xWoken);
    portYIELD_FROM_ISR(xWoken);
}

// Deferred task:
static void deferred_handler(void *params) {
    for (;;) {
        ulTaskNotifyTake(pdTRUE, portMAX_DELAY);  // wait for ISR signal
        while (descriptor_available()) {
            NetworkBufferDescriptor_t *buf = pxGetNetworkBufferWithLength(len);
            copy_frame_to_buffer(buf);
            xSendEventStructToIPTask(buf);  // queue to IP task
        }
    }
}
```
**ISR-to-stack sequence**:
1. **[ISR]** Notify deferred task via `xTaskNotifyFromISR()`
2. **[DEFERRED TASK]** Wake, read DMA ring, allocate buffer, queue to IP task
3. **[IP TASK]** Process ARP/IP/TCP

---

## 5. smoltcp (Rust) - Pure Pull Model

```rust
pub trait Device {
    type RxToken<'a>: RxToken;
    type TxToken<'a>: TxToken;

    fn receive(&mut self, timestamp: Instant)
        -> Option<(Self::RxToken<'_>, Self::TxToken<'_>)>;
    fn transmit(&mut self, timestamp: Instant)
        -> Option<Self::TxToken<'_>>;
    fn capabilities(&self) -> DeviceCapabilities;
}

pub trait RxToken {
    fn consume<R, F: FnOnce(&[u8]) -> R>(self, f: F) -> R;  // temp borrow of packet
}
pub trait TxToken {
    fn consume<R, F: FnOnce(&mut [u8]) -> R>(self, f: F) -> R;  // fill TX buffer
}

pub struct DeviceCapabilities {
    pub medium: Medium,                    // Ethernet, IP, IEEE802154
    pub max_transmission_unit: usize,
    pub checksum: ChecksumCapabilities,    // per-protocol HW checksum support
}
```
**No interrupts/callbacks.** Application polls `device.receive()` in a loop. If HW uses interrupts, ISR sets a flag and `receive()` checks it. Zero-copy via token borrows. Backpressure: `receive()`/`transmit()` return `None`.

---

## 6. embassy-net (Rust async embedded)

```rust
// Driver channel model (simplified):
// ISR/driver task:
loop {
    select! {
        _ = interrupt.wait() => {
            let packet = read_from_hardware();
            rx.rx_done(PacketBuf::new(packet)).await;  // deliver to stack
        }
        tx_buf = tx.poll_transmit() => {
            send_to_hardware(tx_buf.as_ref()).await;
            tx.tx_done().await;
        }
    }
}
```
Async/await replaces callbacks. Waker-based integration with ISRs. Executor schedules processing (like NAPI softirq but for async Rust).

---

## 7. TinyGo netdev

```go
type Netlinker interface {
    SendPacket(pkt []byte) error    // TX: direct call
    RecvPacket() ([]byte, error)    // RX: blocking pull
    HardwareAddr() [6]byte
    MTU() int
}
```
Pure poll/pull model. No interrupt integration. Drivers poll HW internally.

---

## 8. Go standard `net.Interface` (informational only)

```go
type Interface struct {
    Index        int
    MTU          int
    Name         string
    HardwareAddr HardwareAddr  // []byte
    Flags        Flags         // FlagUp | FlagBroadcast | FlagLoopback | FlagPointToPoint | FlagMulticast | FlagRunning
}
```

---

## Universal Pattern: ALL Stacks Use Deferred Processing

Every production stack (Linux, Zephyr, lwIP, FreeRTOS) defers packet processing from ISR context:

| Stack | ISR does | Deferred context | Delivery mechanism |
|-------|----------|-----------------|-------------------|
| Linux NAPI | `napi_schedule()` | softirq (`net_rx_action`) | `napi_gro_receive(skb)` |
| Zephyr | `net_recv_data()` | network task (k_fifo) | `net_if_recv_data()` |
| lwIP | `tcpip_input()` | tcpip_thread (mailbox) | `ethernet_input()` |
| FreeRTOS | `xTaskNotifyFromISR()` | deferred task | `xSendEventStructToIPTask()` |
| smoltcp | set flag | application loop | `device.receive()` poll |
| embassy-net | wake executor | async task | `rx.rx_done()` |

**The ISR never processes the full protocol stack.** It either queues a notification (Linux/Zephyr/FreeRTOS/lwIP) or sets a flag (smoltcp).

---

# Part 2: Interface Design Options

## What lneto needs from a device (data plane only)
- **RX**: Feed raw Ethernet frames into `StackAsync.Demux(pkt, 0)`
- **TX**: Get frames from `StackAsync.Encapsulate(buf, -1, 0)` and send them
- **Config**: MAC address `[6]byte`, MTU `uint16`, optional CRC32 function

## Current device surfaces

| Method | CYW43439 | LAN8720 |
|--------|----------|---------|
| Send frame | `SendEth(pkt []byte) error` | `SendFrame(frame []byte) error` |
| Receive | `RecvEthHandle(cb)` + `PollOne()` (poll triggers callback) | `SetRxHandler(buf, cb)` + `StartRxSingle()` (IRQ triggers callback) |
| MAC | `HardwareAddr6() ([6]byte, error)` from firmware | None (PHY-only, app provides) |
| MTU | `MTU() int` returns 1500 | None (constant in stack code) |
| Link state | `NetFlags() net.Flags`, `IsLinkUp() bool` | `WaitAutoNegotiation()` at init only |
| Frame size | `MaxFrameSize` constant = 2030 | `MTU + ethernet.MaxOverheadSize` |
| CRC | Firmware handles CRC | Needs software CRC from lneto |

---

## Option A: Pull Model (smoltcp-inspired)

Stack calls device to pull data. No callbacks in the interface.

```go
type EthernetDevice interface {
    // RX: Stack pulls a frame. Returns (0, nil) if none available.
    RecvEthFrame(dst []byte) (n int, err error)
    // TX: Stack pushes a frame.
    SendEthFrame(frame []byte) error

    HardwareAddr6() ([6]byte, error)
    MTU() int
    MaxFrameSize() int
}
```

**Stack wrapper RecvAndSend**:
```go
n, _ := dev.RecvEthFrame(rxbuf)
if n > 0 { stack.Demux(rxbuf[:n], 0) }
send, _ := stack.Encapsulate(sendbuf, -1, 0)
if send > 0 { dev.SendEthFrame(sendbuf[:send]) }
```

**CYW43439 impl**: `RecvEthFrame` calls `tryPoll()` internally. If DATA packet, copies ethernet frame to `dst`. If CONTROL/EVENT, processes internally, returns (0, nil).

**LAN8720 adapter**: ISR sets `rxgot` flag. `RecvEthFrame` checks flag, copies from internal buffer to `dst`, rearms `StartRxSingle()`.

| Pro | Con |
|-----|-----|
| Simplest interface (5 methods) | One copy from device buffer to dst |
| Clear buffer ownership | CYW43439 needs internal refactoring (tryPoll flow) |
| No callbacks | Latency tied to polling frequency |
| Identical to smoltcp model | |

---

## Option B: Callback + Poll Model (Linux NAPI / lwIP inspired)

Device delivers frames via registered callback. Poll triggers processing for non-interrupt devices.

```go
type EthernetDevice interface {
    // TX: Stack pushes a frame (software function call).
    SendEthFrame(frame []byte) error
    // RX: Register handler called when frame arrives.
    // Called from Poll() context (never from raw ISR).
    // pkt is only valid for duration of the call (zero-copy).
    SetRecvHandler(handler func(pkt []byte) error)
    // Poll services the device. For poll-based devices (CYW43439),
    // reads from bus and may invoke the handler. For interrupt-driven
    // devices (LAN8720), checks deferred flag and invokes handler.
    // Returns true if handler was invoked.
    Poll() (bool, error)

    HardwareAddr6() ([6]byte, error)
    MTU() int
    MaxFrameSize() int
}
```

**Stack wrapper init + RecvAndSend**:
```go
// Init (once):
dev.SetRecvHandler(func(pkt []byte) error {
    return stack.s.Demux(pkt, 0)
})

// Main loop:
dev.Poll()  // may invoke handler → Demux
send, _ := stack.s.Encapsulate(sendbuf, -1, 0)
if send > 0 { dev.SendEthFrame(sendbuf[:send]) }
```

**CYW43439 impl**:
- `SetRecvHandler` = existing `RecvEthHandle`
- `Poll()` = existing `PollOne()` (reads SPI, processes rx pipeline, calls handler for DATA packets)

**LAN8720 adapter**:
- `SetRecvHandler` stores handler
- ISR sets `rxgot` flag (deferred, not calling handler from ISR)
- `Poll()` checks `rxgot > 0`, calls `handler(rxbuf[:n])`, rearms `StartRxSingle()`

| Pro | Con |
|-----|-----|
| Zero-copy possible (handler borrows device buffer) | 6 methods (SetRecvHandler + Poll + Send + 3 props) |
| Mirrors how both devices already work | Callback requires init ordering (handler must be set before Poll) |
| Matches Linux/lwIP/Zephyr pattern | Handler closure captures stack reference |
| No internal CYW43439 refactoring needed | Slightly more complex interface |
| Natural for interrupt-driven hardware | |

---

## Option C: Hybrid (RecvEthFrame + optional interrupt delivery)

Pull-based with an optional signal mechanism for interrupt-driven devices to notify the stack wrapper.

```go
type EthernetDevice interface {
    RecvEthFrame(dst []byte) (n int, err error)
    SendEthFrame(frame []byte) error
    HardwareAddr6() ([6]byte, error)
    MTU() int
    MaxFrameSize() int
}

// Optional: interrupt-driven devices implement this to signal data ready.
// Stack wrapper can use this to avoid busy-polling.
type HasPendingSignal interface {
    // PendingChan returns a channel that receives when a frame may be ready.
    // RecvEthFrame should still be called to get the actual data.
    PendingChan() <-chan struct{}
}
```

Not recommended for TinyGo embedded due to channel overhead.

---

## Comparison of Options

| Aspect | Option A (Pull) | Option B (Callback+Poll) |
|--------|-----------------|-------------------------|
| Method count | 5 | 6 |
| Buffer ownership | Explicit (caller provides dst) | Implicit (handler borrows device buf) |
| Zero-copy RX | No (copy to dst) | Yes (handler sees device buffer) |
| CYW43439 changes | Add RecvEthFrame, modify tryPoll flow | Minimal (rename existing methods) |
| LAN8720 adapter | RecvEthFrame checks flag, copies, rearms | Poll checks flag, calls handler, rearms |
| Stack wrapper | RecvEthFrame + Demux | SetRecvHandler(Demux) + Poll |
| Closest analogy | smoltcp, TinyGo netdev | Linux NAPI, lwIP, Zephyr |
