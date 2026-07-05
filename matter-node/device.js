/**
 * Official matter.js Matter node (device) for the multi-protocol gateway demo.
 *
 * It exposes TWO On/Off Plug-in Units — one per backend device behind the
 * gateway:
 *   - "inverter" -> gateway target "inverter" (EEBUS)
 *   - "modbus"   -> gateway target "modbus"   (Modbus TCP)
 *
 * Turning a switch ON sets that device's production limit; OFF clears it. The
 * Matter side never knows the field protocol — the gateway routes by target. On
 * startup each switch is aligned to its device's real state read from the
 * gateway.
 *
 * Flow: Matter controller --Matter--> this node --HTTP--> gateway --(EEBUS|Modbus)--> device
 */
import { ServerNode } from "@matter/main";
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

async function readStatus() {
    const res = await fetch(`${GATEWAY_URL}/status`);
    if (!res.ok) {
        throw new Error(`gateway status ${res.status}`);
    }
    return res.json();
}

const node = await ServerNode.create({
    id: "eebus-matter-node",
    network: { port: 5540 },
    commissioning: { passcode: 20202021, discriminator: 3840 },
    productDescription: {
        name: "Gateway Limit Switches",
        deviceType: OnOffPlugInUnitDevice.deviceType,
    },
    basicInformation: {
        vendorName: "Demo",
        vendorId: 0xfff1,
        productName: "Gateway Limit Switches",
        productId: 0x8000,
    },
});

// addSwitch creates an On/Off endpoint bound to a gateway target and returns a
// function that aligns the switch to that device's real state at startup.
async function addSwitch(id, target) {
    const sw = await node.add(OnOffPlugInUnitDevice, { id });
    let syncing = false;

    sw.events.onOff.onOff$Changed.on(async on => {
        if (syncing) {
            return;
        }
        if (on) {
            console.log(`[${target}] Matter ON  -> set production limit ${LIMIT_W} W`);
            await callGateway({ target, watts: LIMIT_W, durationSeconds: DURATION_S });
        } else {
            console.log(`[${target}] Matter OFF -> clear production limit`);
            await callGateway({ target, reset: true });
        }
    });

    return async () => {
        try {
            const all = await readStatus();
            const status = all[target] ?? {};
            const shouldBeOn = status.lastLimitW !== null && status.lastLimitW !== undefined;
            syncing = true;
            await sw.set({ onOff: { onOff: shouldBeOn } });
            syncing = false;
            console.log(`[${target}] initial state: ${shouldBeOn ? `ON (limit ${status.lastLimitW} W)` : "OFF (no limit)"}`);
        } catch (err) {
            syncing = false;
            console.error(`[${target}] could not read initial state: ${err.message}`);
        }
    };
}

// Endpoint 1 = inverter (EEBUS), endpoint 2 = modbus.
const syncInverter = await addSwitch("inverter", "inverter");
const syncModbus = await addSwitch("modbus", "modbus");

node.lifecycle.online.on(async () => {
    await syncInverter();
    await syncModbus();
});

await node.run();
