// Copyright (c) 2017-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

// Plays a sound when someone else starts a Jitsi meeting.
//
// Direct and group messages: a ringtone repeated every RING_EVERY_MS until the user opens the
// conversation, clicks "Join Meeting", or RING_MAX_MS pass. Channels: a single short chime.
// Silent for the user's own meetings, Do Not Disturb / Out of Office and muted channels.
// Admins turn it on or off with the JitsiRingOnMeetingStart setting, users with
// "/jitsi settings ring [true/false]"; the server combines both into ring_on_meeting_start.
//
// Sounds are synthesized with WebAudio, so the plugin ships no audio files.

import manifest from '../manifest';

export const RING_EVERY_MS = 3000;
export const RING_MAX_MS = 30000;
const VOLUME = 0.25;

type Note = [number, number, number]; // frequency (Hz), offset (s), duration (s)

// E5-G5-C6 twice
const RINGTONE: Note[] = [[659, 0, 0.35], [784, 0.15, 0.35], [1047, 0.3, 0.6], [659, 0.9, 0.35], [784, 1.05, 0.35], [1047, 1.2, 0.6]];

// G5-C6
const CHIME: Note[] = [[784, 0, 0.4], [1047, 0.18, 0.7]];

export type RingDecision = {
    kind: 'ring' | 'chime',
    channelId: string,
};

type WebSocketMessage = {
    data?: {
        post?: string | {[key: string]: any},
        channel_type?: string,
    },
};

export function ringEnabled(state: any): boolean {
    const config = state && state[`plugins-${manifest.id}`] && state[`plugins-${manifest.id}`].config;
    return Boolean(config && config.ring_on_meeting_start);
}

// decide returns what to play for a "posted" websocket event, or null for silence.
export function decide(state: any, msg: WebSocketMessage): RingDecision | null {
    const data = msg && msg.data;
    if (!data || !data.post) {
        return null;
    }

    let post: {[key: string]: any};
    try {
        post = typeof data.post === 'string' ? JSON.parse(data.post) : data.post;
    } catch (e) {
        return null;
    }
    if (!post || post.type !== 'custom_jitsi') {
        return null;
    }

    if (!ringEnabled(state)) {
        return null;
    }

    const users = state.entities.users;
    const currentUserId = users.currentUserId;
    if (!currentUserId || post.user_id === currentUserId) {
        return null;
    }

    const status = users.statuses && users.statuses[currentUserId];
    if (status === 'dnd' || status === 'ooo') {
        return null;
    }

    const member = state.entities.channels.myMembers && state.entities.channels.myMembers[post.channel_id];
    if (member && member.notify_props && member.notify_props.mark_unread === 'mention') {
        return null; // muted channel
    }

    const type = data.channel_type;
    return {
        kind: type === 'D' || type === 'G' ? 'ring' : 'chime',
        channelId: post.channel_id,
    };
}

type Ring = {
    channelId: string,
    started: number,
    timer: any,
    output: GainNode | null,
};

export type RingerDeps = {
    getState: () => any,
    createAudioContext?: () => AudioContext | null,
    isWindowFocused?: () => boolean,
    now?: () => number,
};

function defaultAudioContext(): AudioContext | null {
    const AC = (window as any).AudioContext || (window as any).webkitAudioContext;
    if (!AC) {
        return null;
    }
    try {
        return new AC();
    } catch (e) {
        return null;
    }
}

function defaultWindowFocused(): boolean {
    return document.visibilityState === 'visible' && document.hasFocus();
}

export class Ringer {
    private deps: Required<RingerDeps>;
    private ctx: AudioContext | null = null;
    private ring: Ring | null = null;

    constructor(deps: RingerDeps) {
        this.deps = {
            createAudioContext: defaultAudioContext,
            isWindowFocused: defaultWindowFocused,
            now: Date.now,
            ...deps,
        };
    }

    // Browsers keep an AudioContext suspended until a user gesture; call this from one.
    unlock = () => {
        const ctx = this.audio();
        if (ctx && ctx.state === 'suspended') {
            ctx.resume();
        }
    };

    handlePosted = (msg: WebSocketMessage) => {
        const decision = decide(this.deps.getState(), msg);
        if (!decision) {
            return;
        }
        if (decision.kind === 'ring') {
            this.startRing(decision);
        } else {
            this.play(CHIME, null);
        }
    };

    // Stops the ringtone when the user is looking at the ringing conversation.
    handleStateChange = () => {
        if (this.ring && this.looking(this.ring.channelId)) {
            this.stop();
        }
    };

    // stop silences the ringtone immediately, cutting off the notes already scheduled.
    stop = () => {
        const ring = this.ring;
        if (!ring) {
            return;
        }
        this.ring = null;
        clearInterval(ring.timer);
        if (ring.output) {
            try {
                ring.output.disconnect();
            } catch (e) {
                // already disconnected
            }
        }
    };

    isRinging(): boolean {
        return this.ring !== null;
    }

    private audio(): AudioContext | null {
        if (!this.ctx) {
            this.ctx = this.deps.createAudioContext();
        }
        return this.ctx;
    }

    private looking(channelId: string): boolean {
        try {
            return this.deps.isWindowFocused() && this.deps.getState().entities.channels.currentChannelId === channelId;
        } catch (e) {
            return false;
        }
    }

    private startRing(decision: RingDecision) {
        this.stop();
        if (this.looking(decision.channelId)) {
            this.play(CHIME, null); // already in the conversation: a chime is enough
            return;
        }

        const ring: Ring = {
            channelId: decision.channelId,
            started: this.deps.now(),
            timer: null,
            output: this.createOutput(),
        };
        this.ring = ring;
        this.play(RINGTONE, ring.output);
        ring.timer = setInterval(() => {
            if (this.ring !== ring) {
                return;
            }
            if (this.deps.now() - ring.started >= RING_MAX_MS || this.looking(ring.channelId)) {
                this.stop();
                return;
            }
            this.play(RINGTONE, ring.output);
        }, RING_EVERY_MS);
    }

    private createOutput(): GainNode | null {
        const ctx = this.audio();
        if (!ctx) {
            return null;
        }
        const gain = ctx.createGain();
        gain.connect(ctx.destination);
        return gain;
    }

    private play(notes: Note[], output: AudioNode | null) {
        const ctx = this.audio();
        if (!ctx) {
            return;
        }
        if (ctx.state === 'suspended') {
            ctx.resume();
        }
        const destination = output || ctx.destination;
        const start = ctx.currentTime + 0.05;
        for (const [freq, offset, duration] of notes) {
            const at = start + offset;
            const osc = ctx.createOscillator();
            const gain = ctx.createGain();
            osc.type = 'sine';
            osc.frequency.value = freq;
            gain.gain.setValueAtTime(0.0001, at);
            gain.gain.exponentialRampToValueAtTime(VOLUME, at + 0.02);
            gain.gain.exponentialRampToValueAtTime(0.0001, at + duration);
            osc.connect(gain);
            gain.connect(destination);
            osc.start(at);
            osc.stop(at + duration + 0.05);
        }
    }
}

let activeRinger: Ringer | null = null;

export function setActiveRinger(ringer: Ringer | null) {
    activeRinger = ringer;
}

// stopRinging is called when the user joins a meeting.
export function stopRinging() {
    if (activeRinger) {
        activeRinger.stop();
    }
}
