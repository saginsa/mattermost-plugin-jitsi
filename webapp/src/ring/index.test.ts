import {jest, describe, expect, it, beforeEach, afterEach} from '@jest/globals';

import {decide, Ringer, RING_EVERY_MS, RING_MAX_MS, setActiveRinger, stopRinging} from './index';

function makeState(overrides: {status?: string, currentChannelId?: string, ring?: boolean} = {}) {
    return {
        'plugins-jitsi': {config: {ring_on_meeting_start: overrides.ring ?? true}},
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

function posted(type: string, userId: string, channelId: string, channelType: string) {
    return {
        data: {
            channel_type: channelType,
            post: JSON.stringify({type, user_id: userId, channel_id: channelId, props: {meeting_id: 'meeting'}})
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

    it('chimes in channels', () => {
        expect(decide(makeState(), posted('custom_jitsi', 'u', 'town', 'O'))).toEqual({kind: 'chime', channelId: 'town'});
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

describe('Ringer', () => {
    let state: any;
    let ctx: FakeAudioContext;
    let focused: boolean;
    let now: number;
    let ringer: Ringer;

    beforeEach(() => {
        jest.useFakeTimers();
        state = makeState();
        ctx = new FakeAudioContext();
        focused = true;
        now = 0;
        ringer = new Ringer({
            getState: () => state,
            createAudioContext: () => ctx as unknown as AudioContext,
            isWindowFocused: () => focused,
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

    it('repeats the ringtone until RING_MAX_MS', () => {
        ringer.handlePosted(posted('custom_jitsi', 'u', 'dm', 'D'));
        expect(ringer.isRinging()).toBe(true);
        const first = ctx.oscillators;
        tick();
        expect(ctx.oscillators).toBeGreaterThan(first);
        while (now < RING_MAX_MS) {
            tick();
        }
        expect(ringer.isRinging()).toBe(false);
    });

    it('plays a single chime in channels', () => {
        ringer.handlePosted(posted('custom_jitsi', 'u', 'town', 'O'));
        expect(ctx.oscillators).toBeGreaterThan(0);
        expect(ringer.isRinging()).toBe(false);
    });

    it('stops at once when the user opens the conversation', () => {
        ringer.handlePosted(posted('custom_jitsi', 'u', 'dm', 'D'));
        state.entities.channels.currentChannelId = 'dm';
        ringer.handleStateChange();
        expect(ringer.isRinging()).toBe(false);
        expect(ctx.disconnected).toBe(1);
    });

    it('keeps ringing while the window is not focused', () => {
        ringer.handlePosted(posted('custom_jitsi', 'u', 'dm', 'D'));
        focused = false;
        state.entities.channels.currentChannelId = 'dm';
        ringer.handleStateChange();
        tick();
        expect(ringer.isRinging()).toBe(true);
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

    it('only chimes when the conversation is already open', () => {
        state.entities.channels.currentChannelId = 'dm';
        ringer.handlePosted(posted('custom_jitsi', 'u', 'dm', 'D'));
        expect(ctx.oscillators).toBeGreaterThan(0);
        expect(ringer.isRinging()).toBe(false);
    });
});
