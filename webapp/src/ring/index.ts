// Copyright (c) 2017-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

// Plays a sound when someone else starts a Jitsi meeting.
//
// Direct and group messages: a ringtone repeated every RING_EVERY_MS until the user opens the
// meeting link in any way (click, middle click, right click to open or copy it) or the ring duration passes (JitsiRingTimeout, ring_timeout in the config,
// DEFAULT_RING_TIMEOUT_S if missing). Opening the conversation does not stop it: the user may
// read the chat without joining. Each user's ring is separate. Public and private channels ring
// the same way when the server marked the post with ring_in_channel (the channel has at most
// JitsiRingMaxChannelMembers members), otherwise they play a single short chime.
// Silent for the user's own meetings, Do Not Disturb / Out of Office, muted channels and posts
// from accounts listed in JitsiRingSilentSenders (the server marks them with ring_silent).
// Admins turn it on or off with the JitsiRingOnMeetingStart setting and exclude accounts
// (e.g. video conference rooms) from hearing it with JitsiRingExcludedUsers and silence the meetings
// of accounts (e.g. bots that post links in advance) with JitsiRingSilentSenders, users with
// "/jitsi settings ring [true/false]"; the server combines them into ring_on_meeting_start.
//
// Sounds are synthesized with WebAudio, so the plugin ships no audio files.

import manifest from '../manifest';

export const RING_EVERY_MS = 3000;
export const DEFAULT_RING_TIMEOUT_S = 60;
const VOLUME = 0.25;

type Note = [number, number, number]; // frequency (Hz), offset (s), duration (s)

// E5-G5-C6 twice
const RINGTONE: Note[] = [[659, 0, 0.35], [784, 0.15, 0.35], [1047, 0.3, 0.6], [659, 0.9, 0.35], [784, 1.05, 0.35], [1047, 1.2, 0.6]];

// G5-C6
const CHIME: Note[] = [[784, 0, 0.4], [1047, 0.18, 0.7]];

export type RingDecision = {
    kind: 'ring' | 'chime',
    channelId: string
};

type WebSocketMessage = {
    data?: {
        post?: string | {[key: string]: any},
        channel_type?: string
    }
};

function pluginConfig(state: any): any {
    return state && state[`plugins-${manifest.id}`] && state[`plugins-${manifest.id}`].config;
}

export function ringEnabled(state: any): boolean {
    const config = pluginConfig(state);
    return Boolean(config && config.ring_on_meeting_start);
}

// ringTimeoutMs returns how long the ringtone plays, from the server setting.
export function ringTimeoutMs(state: any): number {
    const config = pluginConfig(state);
    const seconds = config && Number(config.ring_timeout);
    return (seconds && seconds > 0 ? seconds : DEFAULT_RING_TIMEOUT_S) * 1000;
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
    if (post.props && post.props.ring_silent) {
        return null; // sent by an account listed in JitsiRingSilentSenders
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
    const ring = type === 'D' || type === 'G' || Boolean(post.props && post.props.ring_in_channel);
    return {
        kind: ring ? 'ring' : 'chime',
        channelId: post.channel_id
    };
}

type Ring = {
    channelId: string,
    started: number,
    timeoutMs: number,
    timer: any,
    output: GainNode | null
};

export type RingerDeps = {
    getState: () => any,
    createAudioContext?: () => AudioContext | null,
    now?: () => number
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

export class Ringer {
    private deps: Required<RingerDeps>;
    private ctx: AudioContext | null = null;
    private ring: Ring | null = null;

    constructor(deps: RingerDeps) {
        this.deps = {
            createAudioContext: defaultAudioContext,
            now: Date.now,
            ...deps
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

    private startRing(decision: RingDecision) {
        this.stop();
        const ring: Ring = {
            channelId: decision.channelId,
            started: this.deps.now(),
            timeoutMs: ringTimeoutMs(this.deps.getState()),
            timer: null,
            output: this.createOutput()
        };
        this.ring = ring;
        this.play(RINGTONE, ring.output);
        ring.timer = setInterval(() => {
            if (this.ring !== ring) {
                return;
            }
            if (this.deps.now() - ring.started >= ring.timeoutMs) {
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
let notifyOtherApps: (() => void) | null = null;

// setActiveRinger registers the ringer and how to tell the user's other apps to stop ringing.
export function setActiveRinger(ringer: Ringer | null, notify: (() => void) | null = null) {
    activeRinger = ringer;
    notifyOtherApps = ringer ? notify : null;
}

// stopRinging is called when the user joins a meeting: stops the ring here and in the
// user's other apps (the server sends them a ring_stop event, see handleRingStopEvent).
export function stopRinging() {
    if (activeRinger) {
        activeRinger.stop();
    }
    if (notifyOtherApps) {
        try {
            notifyOtherApps();
        } catch (e) {
            // the local ring is already stopped
        }
    }
}
