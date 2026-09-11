'use strict';

const net = require('node:net');
const {TextDecoder} = require('node:util');

function createAdbInspection(adb, check, fail) {
  const serial = adb.curDeviceId;
  const args = structuredClone(adb.executable?.defaultArgs);
  if (typeof serial !== 'string' || !/^[A-Za-z0-9._:-]{1,256}$/.test(serial) ||
      !Array.isArray(args) || args.length !== 4 || args[0] !== '-P' ||
      typeof args[1] !== 'string' || !/^[1-9]\d{0,4}$/.test(args[1]) || Number(args[1]) > 65535 || args[2] !== '-s' || args[3] !== serial ||
      adb.listenAllNetwork || adb.remoteAdbPort != null || (adb.adbHost != null && adb.adbHost !== '127.0.0.1') ||
      (adb.remoteAdbHost != null && adb.remoteAdbHost !== '127.0.0.1') ||
      (adb.adbPort != null && adb.adbPort !== Number(args[1])) ||
      process.env.ADB_SERVER_SOCKET || process.env.ANDROID_ADB_SERVER_ADDRESS || process.env.ANDROID_ADB_SERVER_PORT) {
    throw fail(new Error('Unsupported original local ADB connection configuration'));
  }
  const port = Number(args[1]);
  const sockets = new Set();
  let failure;
  let busy = false;
  const cancel = (error) => {
    failure ||= error;
    for (const socket of sockets) socket.destroy(failure);
  };
  const guard = () => {
    if (failure) throw failure;
    check();
    if (adb.curDeviceId !== serial || JSON.stringify(adb.executable?.defaultArgs) !== JSON.stringify(args) ||
        adb.listenAllNetwork || adb.remoteAdbPort != null || (adb.adbHost != null && adb.adbHost !== '127.0.0.1') ||
      (adb.remoteAdbHost != null && adb.remoteAdbHost !== '127.0.0.1') ||
        (adb.adbPort != null && adb.adbPort !== port) || process.env.ADB_SERVER_SOCKET || process.env.ANDROID_ADB_SERVER_ADDRESS || process.env.ANDROID_ADB_SERVER_PORT) throw new Error('Original ADB endpoint changed');
  };
  const request = (service, shell, deadline, binary = false) => new Promise((resolve, reject) => {
    let socket;
    let timer;
    let settled = false;
    let pending = Buffer.alloc(0);
    let stage = shell ? 'transport' : 'status';
    let stdout = Buffer.alloc(0);
    let stderr = 0;
    let output;
    let frames = 0;
    const finish = (error) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      if (error) { cancel(error); fail(error); }
      socket?.destroy();
      if (error) reject(error); else resolve(output);
    };
    const live = () => {
      guard();
      if (!Number.isSafeInteger(deadline) || Date.now() >= deadline) throw new Error('ADB observation deadline');
    };
    const send = (value) => {
      live();
      const bytes = Buffer.from(value);
      socket.write(Buffer.concat([Buffer.from(bytes.length.toString(16).padStart(4, '0')), bytes]));
    };
    try {
      live();
      socket = net.createConnection({host: '127.0.0.1', port});
      sockets.add(socket);
      timer = setTimeout(() => finish(new Error('ADB observation deadline')), deadline - Date.now());
      socket.on('connect', () => { try { send(shell ? `host:transport:${serial}` : service); } catch (error) { finish(error); } });
      socket.on('data', (chunk) => {
        if (settled) return;
        try {
          live();
          if (!Buffer.isBuffer(chunk)) throw new Error('Unexpected ADB response encoding');
          if (pending.length + chunk.length > 1048581) throw new Error('ADB response bound');
          pending = Buffer.concat([pending, chunk]);
          while (pending.length) {
            live();
            if (stage === 'done') throw new Error('ADB trailing response');
            if (stage === 'transport' || stage === 'status') {
              if (pending.length < 4) return;
              const status = pending.subarray(0, 4).toString('latin1');
              pending = pending.subarray(4);
              if (status === 'FAIL') { stage = 'failure'; continue; }
              if (status !== 'OKAY') throw new Error('ADB malformed status');
              if (stage === 'transport') {
                if (pending.length) throw new Error('ADB premature shell response');
                stage = 'status'; send(service); return;
              }
              stage = shell ? 'frame' : 'length';
            } else if (stage === 'length' || stage === 'failure') {
              if (pending.length < 4) return;
              const text = pending.subarray(0, 4).toString('latin1');
              if (!/^[0-9a-fA-F]{4}$/.test(text)) throw new Error('ADB malformed length');
              const length = parseInt(text, 16);
              if (pending.length < length + 4) return;
              if (stage === 'failure') throw new Error('ADB service refused');
              output = new TextDecoder('utf-8', {fatal: true}).decode(pending.subarray(4, length + 4));
              pending = pending.subarray(length + 4); stage = 'done';
            } else {
              if (pending.length < 5) return;
              const id = pending[0];
              const length = pending.readUInt32LE(1);
              if (![1, 2, 3].includes(id) || length > 1048576 || (id === 3 && length !== 1)) throw new Error('ADB shell frame header');
              if (pending.length < length + 5) return;
              if (++frames > 4096) throw new Error('ADB shell frame count');
              const body = pending.subarray(5, length + 5);
              pending = pending.subarray(length + 5);
              if (id === 1) {
                if (stdout.length + length > (binary ? 262144 : 1048576)) throw new Error('ADB stdout bound');
                stdout = Buffer.concat([stdout, body]);
              } else if (id === 2) {
                stderr += length;
                if (stderr > 65536) throw new Error('ADB stderr bound');
              } else {
                if (body[0] !== 0 || stderr) throw new Error('ADB shell unsuccessful completion');
                output = binary ? stdout : new TextDecoder('utf-8', {fatal: true}).decode(stdout);
                stage = 'done';
              }
            }
          }
        } catch (error) { finish(error); }
      });
      socket.on('error', finish);
      socket.on('end', () => { try { live(); if (stage !== 'done' || pending.length) throw new Error('ADB partial response'); finish(); } catch (error) { finish(error); } });
      socket.on('close', () => { sockets.delete(socket); if (!settled) finish(new Error('ADB connection lost')); });
    } catch (error) { finish(error); }
  });
  return {
    cancel,
    async readKernelConfig(deadline) {
      try {
        guard();
        if (busy || !Number.isSafeInteger(deadline) || deadline <= Date.now() || deadline - Date.now() > 30000) throw new Error('Invalid ADB read admission');
        busy = true;
        if (await request('host:version', false, deadline) !== '0029') throw new Error('Unsupported existing ADB server version');
        return await request('shell,v2,raw:cat /proc/config.gz', true, deadline, true);
      } catch (error) { cancel(error); throw fail(error); }
      finally { busy = false; }
    },
    async read(args, deadline) {
      try {
        guard();
        if (busy || !Number.isSafeInteger(deadline) || deadline <= Date.now() || deadline - Date.now() > 30000) throw new Error('Invalid ADB read admission');
        busy = true;
        if (!Array.isArray(args)) throw new Error('Unsupported fixed ADB read');
        const key = JSON.stringify(args);
        const forward = key === '["forward","--list"]';
        const fixed = ['pidof com.android.chrome', 'dumpsys activity activities', 'cat /proc/net/unix', 'cat /proc/self/status', 'cat /proc/sys/kernel/random/boot_id'];
        const command = args.slice(1).join(' ');
        if (!forward && (args[0] !== 'shell' || args.some(value => typeof value !== 'string') ||
            (!fixed.includes(command) && !/^cat \/proc\/[1-9]\d{0,9}\/(?:stat|status)$/.test(command)) ||
            JSON.stringify(command.split(' ')) !== JSON.stringify(args.slice(1)))) throw new Error('Unsupported fixed ADB read');
        if (await request('host:version', false, deadline) !== '0029') throw new Error('Unsupported existing ADB server version');
        if (forward) return (await request('host:list-forward', false, deadline)).trim();
        return (await request(`shell,v2,raw:${command}`, true, deadline)).trim();
      } catch (error) { cancel(error); throw fail(error); }
      finally { busy = false; }
    },
  };
}

module.exports = {createAdbInspection};
