(function () {
  const mock = {
    microphoneRequests: 0,
    stoppedTracks: 0,
    recorderStarts: 0,
    recorderStops: 0,
    dataOnlyOnStop: false,
    uploadCount: 0,
    uploadedBodies: [],
    chatCount: 0,
    reset() {
      this.microphoneRequests = 0;
      this.stoppedTracks = 0;
      this.recorderStarts = 0;
      this.recorderStops = 0;
      this.dataOnlyOnStop = false;
      this.uploadCount = 0;
      this.uploadedBodies = [];
      this.chatCount = 0;
    },
  };

  class MockTrack extends EventTarget {
    constructor(kind) {
      super();
      this.kind = kind;
      this.readyState = "live";
    }
    stop() {
      if (this.readyState === "ended") return;
      this.readyState = "ended";
      mock.stoppedTracks++;
    }
  }

  class MockStream {
    constructor(tracks) {
      this.tracks = tracks || [];
    }
    getTracks() {
      return this.tracks;
    }
    getAudioTracks() {
      return this.tracks.filter((track) => track.kind === "audio");
    }
    getVideoTracks() {
      return this.tracks.filter((track) => track.kind === "video");
    }
  }

  class MockMediaRecorder {
    static isTypeSupported() {
      return true;
    }
    constructor(stream, options) {
      this.state = "inactive";
      this.mimeType = (options && options.mimeType) || "audio/webm";
      this.ondataavailable = null;
      this.onstop = null;
      this.onerror = null;
    }
    start(timeslice) {
      if (timeslice !== 1000)
        throw new Error("unexpected timeslice " + timeslice);
      mock.recorderStarts++;
      this.state = "recording";
      if (mock.dataOnlyOnStop) return;
      queueMicrotask(() => this.emitChunk("first", true));
      queueMicrotask(() => this.emitChunk("second", false));
    }
    stop() {
      mock.recorderStops++;
      this.state = "inactive";
      this.emitChunk(
        mock.dataOnlyOnStop ? "encodedlast" : "last",
        mock.dataOnlyOnStop,
      );
      queueMicrotask(() => {
        if (this.onstop) this.onstop();
      });
    }
    emitChunk(value, withWebMHeader) {
      const event = new Event("dataavailable");
      Object.defineProperty(event, "data", {
        value: new Blob(
          [
            withWebMHeader
              ? new Uint8Array([0x1a, 0x45, 0xdf, 0xa3])
              : new Uint8Array(),
            value,
          ],
          { type: this.mimeType },
        ),
      });
      if (this.ondataavailable) this.ondataavailable(event);
    }
  }

  class MockAudioContext {
    constructor() {
      this.state = "running";
    }
    createMediaStreamDestination() {
      return { stream: new MockStream([new MockTrack("audio")]) };
    }
    createMediaStreamSource() {
      return { connect() {}, disconnect() {} };
    }
    createAnalyser() {
      return {
        fftSize: 64,
        smoothingTimeConstant: 0,
        disconnect() {},
        getByteTimeDomainData(samples) {
          samples.fill(128);
        },
      };
    }
    async resume() {}
    async close() {}
  }

  Object.defineProperty(window, "MediaStream", {
    configurable: true,
    value: MockStream,
  });
  Object.defineProperty(window, "MediaRecorder", {
    configurable: true,
    value: MockMediaRecorder,
  });
  Object.defineProperty(window, "AudioContext", {
    configurable: true,
    value: MockAudioContext,
  });
  Object.defineProperty(navigator, "mediaDevices", {
    configurable: true,
    value: {
      async getUserMedia() {
        mock.microphoneRequests++;
        return new MockStream([new MockTrack("audio")]);
      },
    },
  });
  // The predictable-only server has no transcription route, which gates
  // recording; chat is mocked below, so report transcription as available.
  let init;
  Object.defineProperty(window, "__SHELLEY_INIT__", {
    configurable: true,
    get: () => init,
    set: (value) => {
      init = { ...value, transcription_available: true };
    },
  });
  Object.defineProperty(window, "__recordingMock", {
    configurable: true,
    value: mock,
  });

  const originalFetch = window.fetch;
  window.fetch = async function (input, init) {
    const request =
      input instanceof Request
        ? new Request(input, init)
        : new Request(new URL(String(input), window.location.href), init);
    const url = new URL(request.url);
    if (request.method === "POST" && url.pathname === "/api/upload/raw") {
      const bytes = await request.arrayBuffer();
      mock.uploadCount++;
      mock.uploadedBodies.push(Array.from(new Uint8Array(bytes)));
      return new Response(
        JSON.stringify({ path: "/tmp/shelley-uploads/lazycue.webm" }),
        {
          status: 200,
          headers: { "Content-Type": "application/json" },
        },
      );
    }
    if (
      request.method === "POST" &&
      /^\/api\/conversation\/[^/]+\/chat$/.test(url.pathname)
    ) {
      mock.chatCount++;
      return new Response(JSON.stringify({ status: "queued" }), {
        status: 202,
        headers: { "Content-Type": "application/json" },
      });
    }
    return originalFetch.apply(this, arguments);
  };
})();
