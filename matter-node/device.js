/**
 * Official matter.js Matter node (device) for the EEBUS gateway demo.
 *
 * It exposes a standard On/Off Plug-in Unit. Any Matter controller (Apple Home,
 * Google Home, chip-tool, matter.js shell/controller) can toggle it:
 *
 *   Matter ON  -> POST /limit { watts: LIMIT_W }   -> gateway writes EG-LPP limit -> inverter curtails
 *   Matter OFF -> POST /limit { reset: true }       -> gateway clears the limit    -> inverter back to normal
 *
 * On startup it reads the inverter's real limit state from the gateway and
 * aligns the switch to it, so the switch reflects reality (not a stale value).
 *
 * Flow: Matter controller --Matter--> this node --HTTP--> Go gateway --EEBUS--> inverter
 */
import { Endpoint, ServerNode } from "@matter/main";
import { OnOffPlugInUnitDevice } from "@matter/main/devices/on-off-plug-in-unit";

const GATEWAY_URL = process.env.GATEWAY_URL ?? "http://127.0.0.1:8090";
const LIMIT_W = Number(process.env.LIMIT_W ?? 3000);
const DURATION_S = Number(process.env.DURATION_S ?? 120);

async function callGateway(body) {
    try {
        const res = await fetch(`${GATEWAY_URL}/limit`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(body),
        });
        console.log(`[gateway] ${res.status} ${await res.text()}`.trim());
    } catch (err) {
        console.error(`[gateway] request failed: ${err.message}`);
    }
}

const node = await ServerNode.create({
    id: "eebus-matter-node",
    network: { port: 5540 },
    commissioning: { passcode: 20202021, discriminator: 3840 },
    productDescription: {
        name: "EEBUS Inverter Limit",
        deviceType: OnOffPlugInUnitDevice.deviceType,
    },
    basicInformation: {
        vendorName: "Demo",
        vendorId: 0xfff1,
        productName: "EEBUS Production Limit Switch",
        productId: 0x8000,
    },
});

const limitSwitch = await node.add(OnOffPlugInUnitDevice, { id: "limit" });

// While we align the switch to the inverter's real state at startup, we must not
// echo that programmatic change back to the gateway.
let syncingFromGateway = false;

limitSwitch.events.onOff.onOff$Changed.on(async on => {
    if (syncingFromGateway) {
        return;
    }
    if (on) {
        console.log(`Matter ON  -> set production limit ${LIMIT_W} W`);
        await callGateway({ watts: LIMIT_W, durationSeconds: DURATION_S });
    } else {
        console.log("Matter OFF -> clear production limit");
        await callGateway({ reset: true });
    }
});

// On startup, read the inverter's real limit state from the gateway and align
// the Matter switch to it, so the switch always reflects reality instead of a
// stale persisted value.
node.lifecycle.online.on(async () => {
    try {
        const res = await fetch(`${GATEWAY_URL}/status`);
        if (!res.ok) {
            throw new Error(`gateway status ${res.status}`);
        }
        const status = await res.json();
        const shouldBeOn = status.lastLimitW !== null && status.lastLimitW !== undefined;
        syncingFromGateway = true;
        await limitSwitch.set({ onOff: { onOff: shouldBeOn } });
        syncingFromGateway = false;
        console.log(
            `initial state read from inverter: ${shouldBeOn ? `ON (limit ${status.lastLimitW} W)` : "OFF (no limit)"}`,
        );
    } catch (err) {
        syncingFromGateway = false;
        console.error(`could not read initial state from gateway: ${err.message}`);
    }
});

await node.run();
