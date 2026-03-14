/*
 * Shared C declarations for the ndndSIM Go simulation bridge.
 * Included by cgo_export.go via CGo preamble.
 */

#ifndef NDNDSIM_SIM_H
#define NDNDSIM_SIM_H

#include <stdint.h>

/* Callback function pointer types (set by C++ during init) */
typedef void (*NdndSimSendPacketFunc)(uint32_t nodeId, uint32_t ifIndex,
                                      const void* data, uint32_t dataLen);
typedef void (*NdndSimScheduleEventFunc)(uint32_t nodeId, int64_t delayNs,
                                          uint64_t eventId);
typedef void (*NdndSimCancelEventFunc)(uint64_t eventId);
typedef int64_t (*NdndSimGetTimeNsFunc)(void);

/* Stored callback pointers */
static NdndSimSendPacketFunc     _sendPacketCb;
static NdndSimScheduleEventFunc  _scheduleEventCb;
static NdndSimCancelEventFunc    _cancelEventCb;
static NdndSimGetTimeNsFunc      _getTimeNsCb;

static inline void setSendPacketCb(NdndSimSendPacketFunc cb)       { _sendPacketCb = cb; }
static inline void setScheduleEventCb(NdndSimScheduleEventFunc cb) { _scheduleEventCb = cb; }
static inline void setCancelEventCb(NdndSimCancelEventFunc cb)     { _cancelEventCb = cb; }
static inline void setGetTimeNsCb(NdndSimGetTimeNsFunc cb)         { _getTimeNsCb = cb; }

static inline void callSendPacket(uint32_t nodeId, uint32_t ifIndex, const void* data, uint32_t dataLen) {
    if (_sendPacketCb) _sendPacketCb(nodeId, ifIndex, data, dataLen);
}

static inline void callScheduleEvent(uint32_t nodeId, int64_t delayNs, uint64_t eventId) {
    if (_scheduleEventCb) _scheduleEventCb(nodeId, delayNs, eventId);
}

static inline void callCancelEvent(uint64_t eventId) {
    if (_cancelEventCb) _cancelEventCb(eventId);
}

static inline int64_t callGetTimeNs(void) {
    if (_getTimeNsCb) return _getTimeNsCb();
    return 0;
}

#endif /* NDNDSIM_SIM_H */
