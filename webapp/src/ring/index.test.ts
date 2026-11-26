import {jest, describe, expect, it, beforeEach, afterEach} from '@jest/globals';

import {decide, DEFAULT_RING_TIMEOUT_S, Ringer, RING_EVERY_MS, ringTimeoutMs, setActiveRinger, stopRinging} from './index';

function makeState(overrides: {status?: string, currentChannelId?: string, ring?: boolean, timeout?: number} = {}) {
    return {
        'plugins-jitsi': {config: {ring_on_meeting_start: overrides.ring ?? true, ring_timeout: overrides.timeout}},
        entities: {
            users: {currentUserId: 'me', statuses: {me: overrides.status || 'online'}},
            channels: {
                currentChannelId: overrides.currentChannelId || 'other',
                myMembers: {
                    dm: {notify_props: {mark_unread: 'all'}},
                    muted: {notify_props: {mark_unread: 'mention'}}
                }
            }
        }
    };
}

function posted(type: string, userId: string, channelId: string, channelType: string, props: {[key: string]: any} = {}) {
    return {
        data: {
            channel_type: channelType,
            post: JSON.stringify({type, user_id: userId, channel_id: channelId, props: {meeting_id: 'meeting', ...props}})
        }
    };
}

class FakeAudioContext {
    state = 'running';
    currentTime = 0;
    destination = {};
    oscillators = 0;
    disconnected = 0;

    createOscillator() {
        this.oscillators++;
        return {frequency: {}, connect: jest.fn(), start: jest.fn(), stop: jest.fn()};
    }

    createGain() {
        return {
            gain: {setValueAtTime: jest.fn(), exponentialRampToValueAtTime: jest.fn()},
            connect: jest.fn(),
            disconnect: () => {
                this.disconnected++;
            }
        };
    }

    resume() {
        return Promise.resolve();
    }
}

describe('decide', () => {
    it('ignores non-Jitsi posts', () => {
        expect(decide(makeState(), posted('', 'u', 'dm', 'D'))).toBeNull();
    });

    it('ignores own meetings', () => {
        expect(decide(makeState(), posted('custom_jitsi', 'me', 'dm', 'D'))).toBeNull();
    });

    it('rings in direct and group messages', () => {
        expect(decide(makeState(), posted('custom_jitsi', 'u', 'dm', 'D'))).toEqual({kind: 'ring', channelId: 'dm'});
        expect(decide(makeState(), posted('custom_jitsi', 'u', 'gm', 'G'))).toEqual({kind: 'ring', channelId: 'gm'});
    });

    it('rings in channels within the member limit, chimes in larger ones', () => {
        expect(decide(makeState(), posted('custom_jitsi', 'u', 'team', 'O', {ring_in_channel: true}))).toEqual({kind: 'ring', channelId: 'team'});
        expect(decide(makeState(), posted('custom_jitsi', 'u', 'team', 'P', {ring_in_channel: true}))).toEqual({kind: 'ring', channelId: 'team'});
        expect(decide(makeState(), posted('custom_jitsi', 'u', 'town', 'O', {ring_in_channel: false}))).toEqual({kind: 'chime', channelId: 'town'});
        expect(decide(makeState(), posted('custom_jitsi', 'u', 'town', 'O'))).toEqual({kind: 'chime', channelId: 'town'});
    });

    it('is silent for meetings from accounts the server marked ring_silent', () => {
        expect(decide(makeState(), posted('custom_jitsi', 'bot', 'dm', 'D', {ring_silent: true}))).toBeNull();
        expect(decide(makeState(), posted('custom_jitsi', 'bot', 'team', 'O', {ring_in_channel: true, ring_silent: true}))).toBeNull();
    });

    it('is silent when turned off, muted, dnd or ooo', () => {
        expect(decide(makeState({ring: false}), posted('custom_jitsi', 'u', 'dm', 'D'))).toBeNull();
        expect(decide(makeState(), posted('custom_jitsi', 'u', 'muted', 'O'))).toBeNull();
        expect(decide(makeState({status: 'dnd'}), posted('custom_jitsi', 'u', 'dm', 'D'))).toBeNull();
        expect(decide(makeState({status: 'ooo'}), posted('custom_jitsi', 'u', 'dm', 'D'))).toBeNull();
    });

    it('survives malformed posts', () => {
        expect(decide(makeState(), {data: {post: '{oops', channel_type: 'D'}})).toBeNull();
        expect(decide(makeState(), {})).toBeNull();
    });
});

describe('ringTimeoutMs', () => {
    it('uses the server setting', () => {
        expect(ringTimeoutMs(makeState({timeout: 45}))).toBe(45000);
    });

    it('falls back to the default', () => {
        expect(ringTimeoutMs(makeState())).toBe(DEFAULT_RING_TIMEOUT_S * 1000);
        expect(ringTimeoutMs(makeState({timeout: 0}))).toBe(DEFAULT_RING_TIMEOUT_S * 1000);
        expect(ringTimeoutMs({})).toBe(DEFAULT_RING_TIMEOUT_S * 1000);
    });
});

describe('Ringer', () => {
    let state: any;
    let ctx: FakeAudioContext;
    let now: number;
    let ringer: Ringer;

    beforeEach(() => {
        jest.useFakeTimers();
        state = makeState();
        ctx = new FakeAudioContext();
        now = 0;
        ringer = new Ringer({
            getState: () => state,
            createAudioContext: () => ctx as unknown as AudioContext,
            now: () => now
        });
    });

    afterEach(() => {
        ringer.stop();
        jest.useRealTimers();
    });

    function tick() {
        now += RING_EVERY_MS;
        jest.advanceTimersByTime(RING_EVERY_MS);
    }

    it('repeats the ringtone for the default duration', () => {
        ringer.handlePosted(posted('custom_jitsi', 'u', 'dm', 'D'));
        expect(ringer.isRinging()).toBe(true);
        const first = ctx.oscillators;
        tick();
        expect(ctx.oscillators).toBeGreaterThan(first);
        while (now < DEFAULT_RING_TIMEOUT_S * 1000) {
            tick();
        }
        expect(ringer.isRinging()).toBe(false);
    });

    it('plays a single chime in channels', () => {
        ringer.handlePosted(posted('custom_jitsi', 'u', 'town', 'O'));
        expect(ctx.oscillators).toBeGreaterThan(0);
        expect(ringer.isRinging()).toBe(false);
    });

    it('keeps ringing when the user opens the conversation', () => {
        ringer.handlePosted(posted('custom_jitsi', 'u', 'dm', 'D'));
        state.entities.channels.currentChannelId = 'dm';
        tick();
        expect(ringer.isRinging()).toBe(true);
    });

    it('rings for the configured duration', () => {
        state = makeState({timeout: 90});
        ringer.handlePosted(posted('custom_jitsi', 'u', 'dm', 'D'));
        while (now < 87000) {
            tick();
        }
        expect(ringer.isRinging()).toBe(true);
        tick();
        expect(ringer.isRinging()).toBe(false);
    });

    it('stops and cuts the sound off when the user joins', () => {
        setActiveRinger(ringer);
        ringer.handlePosted(posted('custom_jitsi', 'u', 'dm', 'D'));
        stopRinging();
        expect(ringer.isRinging()).toBe(false);
        expect(ctx.disconnected).toBe(1);
        const played = ctx.oscillators;
        tick();
        expect(ctx.oscillators).toBe(played);
        setActiveRinger(null);
    });

    it('tells the other apps to stop when the user joins', () => {
        const notify = jest.fn();
        setActiveRinger(ringer, notify);
        ringer.handlePosted(posted('custom_jitsi', 'u', 'dm', 'D'));
        stopRinging();
        expect(ringer.isRinging()).toBe(false);
        expect(notify).toHaveBeenCalledTimes(1);
        setActiveRinger(null, notify);
        stopRinging();
        expect(notify).toHaveBeenCalledTimes(1);
    });

    it('stops on a ring_stop event from another app', () => {
        ringer.handlePosted(posted('custom_jitsi', 'u', 'dm', 'D'));
        ringer.stop();
        expect(ringer.isRinging()).toBe(false);
        expect(ctx.disconnected).toBe(1);
    });

    it('rings even when the conversation is already open', () => {
        state.entities.channels.currentChannelId = 'dm';
        ringer.handlePosted(posted('custom_jitsi', 'u', 'dm', 'D'));
        expect(ringer.isRinging()).toBe(true);
    });
});
