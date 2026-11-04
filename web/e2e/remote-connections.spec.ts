import { expect, test } from "@playwright/test";
import { mockFleet } from "./fixture";

test("Discord pairing is private and the bot token leaves the form after submission",async({page})=>{
 await mockFleet(page);
 const state={revision:0,recovery:true,messages:[],lessons:[],listenerAt:"",telegram:{configured:false},discord:{configured:false,paired:false,bot:"",status:""}};
 const writes:unknown[]=[];
 await page.route("**/api/mastermind/control**",route=>{
  const req=route.request();if(req.method()==="GET")return route.fulfill({json:state});
  writes.push(req.postDataJSON());state.discord={configured:true,paired:false,bot:"fixture_bot",status:"Waiting for pairing"};
  return route.fulfill({json:{bot:"fixture_bot",pairCommand:"!pair fixture-code",expiresAt:new Date(Date.now()+600000).toISOString()}});
 });
 await page.goto("/#mastermind");await page.locator(".mastermind-control summary").first().click();
 await page.getByRole("navigation",{name:"Mastermind control",exact:true}).getByRole("button",{name:"Discord",exact:true}).click();
 await page.getByLabel("Discord bot token").fill("fixture-bot-token");await page.getByLabel("Your Discord user ID").fill("123456789");
 await page.getByRole("button",{name:"Connect Discord",exact:true}).click();
 await expect(page.getByLabel("Discord pairing command")).toHaveValue("!pair fixture-code");await expect(page.getByLabel("Discord bot token")).toHaveCount(0);
 await expect(page.locator(".control-body")).not.toContainText("fixture-bot-token");expect(writes).toEqual([{token:"fixture-bot-token",userId:"123456789"}]);
 await expect(page.getByLabel("Discord connection")).toContainText("Server channels, webhooks, and other users are ignored");
});

test("personal WhatsApp links a device explicitly and presents a short-lived code",async({page})=>{
 const fleet=await mockFleet(page);const state={configured:false,paired:false,connected:false,status:""};let posts=0;
 await page.route("**/api/mastermind/control/whatsapp-personal",route=>{
  if(route.request().method()==="GET")return route.fulfill({json:state});
  posts++;expect(route.request().postDataJSON()).toEqual({phone:"+19995550123"});state.configured=true;state.status="Waiting for phone link";
  return route.fulfill({json:{code:"ABCD-EFGH",expiresAt:new Date(Date.now()+180000).toISOString(),status:state}});
 });
 await page.goto("/#mastermind");await page.locator(".mastermind-control summary").first().click();
 await page.getByRole("navigation",{name:"Mastermind control",exact:true}).getByRole("button",{name:"WhatsApp",exact:true}).click();
 await page.getByLabel("WhatsApp phone number").fill("+19995550123");await page.getByRole("button",{name:"Link personal WhatsApp",exact:true}).click();
 await expect(page.getByLabel("WhatsApp device link code")).toHaveValue("ABCD-EFGH");await expect(page.getByLabel("WhatsApp phone number")).toHaveCount(0);
 await expect(page.getByLabel("Personal WhatsApp connection")).toContainText("!relay allow ID");expect(posts).toBe(1);expect(fleet.mutations).toEqual([]);
});

test("WhatsApp Business keeps separate credentials and requires explicit bridge settings",async({page})=>{
 await mockFleet(page);const state={configured:false,connected:false,status:""};const writes:unknown[]=[];
 await page.route("**/api/mastermind/control/whatsapp-personal",route=>route.fulfill({json:{configured:false,status:""}}));
 await page.route("**/api/mastermind/control/whatsapp-business",route=>{
  if(route.request().method()==="GET")return route.fulfill({json:state});writes.push(route.request().postDataJSON());state.configured=true;state.connected=true;state.status="Bridge connected";return route.fulfill({json:state});
 });
 await page.goto("/#mastermind");await page.locator(".mastermind-control summary").first().click();await page.getByRole("navigation",{name:"Mastermind control",exact:true}).getByRole("button",{name:"WhatsApp",exact:true}).click();
 await page.getByRole("button",{name:"Business Cloud API",exact:true}).click();
 const connect=page.getByRole("button",{name:"Connect WhatsApp Business",exact:true});await expect(connect).toBeDisabled();
 await page.getByLabel("Webhook bridge URL").fill("https://fixture.example/relay");await page.getByLabel("Bridge access token",{exact:true}).fill("fixture-bridge-secret");await page.getByLabel("Meta Cloud access token").fill("fixture-cloud-secret");await page.getByLabel("Meta phone-number ID").fill("123456");await page.getByLabel("Allowed sender number").fill("19995550123");await page.getByLabel("Meta Graph API version").fill("v99.0");
 await connect.click();await expect(page.getByLabel("WhatsApp Business connection")).toContainText("Bridge connected");
 expect(writes).toEqual([{bridgeUrl:"https://fixture.example/relay",bridgeToken:"fixture-bridge-secret",accessToken:"fixture-cloud-secret",phoneNumberId:"123456",owner:"19995550123",graphVersion:"v99.0"}]);
 await expect(page.getByLabel("Meta Cloud access token")).toHaveCount(0);await expect(page.locator(".control-body")).not.toContainText("fixture-cloud-secret");
 expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);
});

test("uncertain connector setup is not repeated automatically and clears sensitive inputs",async({page})=>{
 await mockFleet(page);let posts=0;
 await page.route("**/api/mastermind/control/whatsapp-personal",route=>{
  if(route.request().method()==="GET")return route.fulfill({json:{configured:false,paired:false,status:""}});posts++;return route.abort("failed");
 });
 await page.goto("/#mastermind");await page.locator(".mastermind-control summary").first().click();await page.getByRole("navigation",{name:"Mastermind control",exact:true}).getByRole("button",{name:"WhatsApp",exact:true}).click();
 await page.getByLabel("WhatsApp phone number").fill("+19995550123");await page.getByRole("button",{name:"Link personal WhatsApp",exact:true}).click();
 await expect(page.getByLabel("Personal WhatsApp connection").getByRole("alert")).toContainText("may have completed");await expect(page.getByLabel("WhatsApp phone number")).toHaveValue("");
 expect(posts).toBe(1);await expect(page.getByRole("button",{name:"Link personal WhatsApp",exact:true})).toBeDisabled();
});
