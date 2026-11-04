import { expect, test } from "@playwright/test";
import { local, mockFleet, remote } from "./fixture";
import type { MastermindState } from "../src/types";
const mastermind:MastermindState={revision:4,enabled:true,status:"running",stage:"Connected",config:{hostId:"local",harness:"claude",agentMode:"manual",cwd:"/fixture"},updatedAt:"2026-11-04T10:00:00Z"};
const plan=(hostId="dev",blockers:string[]=[])=>({hostId,fingerprint:"f".repeat(64),sourceRevision:4,harness:"claude",agentMode:"manual",counts:{pages:4,tasks:3,lessons:2,queuedMessages:1},blockers,warnings:["Configure remote-control channels on the destination"]});

test("migration checks the destination before an explicitly confirmed single handoff",async({page})=>{
 const fleet=await mockFleet(page,{mastermind});let operation:object|undefined;const writes:{path:string;body:any}[]=[];
 await page.route("**/api/mastermind/migration**",route=>{const req=route.request();const path=new URL(req.url()).pathname;if(req.method()==="GET")return route.fulfill({json:{operation}});const body=req.postDataJSON();writes.push({path,body});if(path.endsWith("/preflight"))return route.fulfill({json:plan()});operation={id:body.id,hostId:body.hostId,status:"preparing",stage:"Preparing destination",updatedAt:new Date().toISOString()};return route.fulfill({json:operation,status:202});});
 await page.goto("/#mastermind");await page.getByRole("button",{name:"Move Mastermind",exact:true}).click();
 const modal=page.getByRole("dialog",{name:"Move Mastermind",exact:true});
 await expect(modal.getByRole("button",{name:"Move and start Mastermind",exact:true})).toBeDisabled();
 await modal.getByRole("button",{name:"Check destination",exact:true}).click();
 await expect(modal).toContainText("fresh conversation");await expect(modal).toContainText("Provider sign-in, SSH keys, and messaging credentials stay");
 await expect(modal.getByRole("button",{name:"Move and start Mastermind",exact:true})).toBeDisabled();
 await modal.getByRole("checkbox").check();await modal.getByRole("button",{name:"Move and start Mastermind",exact:true}).click();
 await expect(modal).toHaveCount(0);await expect(page.getByLabel("Mastermind location")).toContainText("Preparing destination");
 expect(writes).toHaveLength(2);expect(writes[0].body).toEqual({hostId:"dev"});expect(writes[1].body).toMatchObject({hostId:"dev",fingerprint:"f".repeat(64)});expect(writes[1].body.id).toMatch(/^[a-f0-9]{32}$/);expect(fleet.mutations).toEqual([]);
});

test("claimed inbox work blocks migration and cannot be overridden in the dialog",async({page})=>{
 await mockFleet(page,{mastermind});let starts=0;
 await page.route("**/api/mastermind/migration**",route=>{if(route.request().method()==="GET")return route.fulfill({json:{}});if(route.request().url().endsWith("/preflight"))return route.fulfill({json:plan("dev",["Finish the processing instruction before moving Mastermind."])});starts++;return route.fulfill({json:{}});});
 await page.goto("/#mastermind");await page.getByRole("button",{name:"Move Mastermind",exact:true}).click();await page.getByRole("button",{name:"Check destination",exact:true}).click();
 const modal=page.getByRole("dialog",{name:"Move Mastermind",exact:true});await expect(modal.getByRole("alert")).toContainText("processing instruction");await expect(modal.getByRole("checkbox")).toBeDisabled();await expect(modal.getByRole("button",{name:"Move and start Mastermind",exact:true})).toBeDisabled();expect(starts).toBe(0);
});

test("unknown migration persists across navigation and an unrelated receipt cannot unlock it",async({page})=>{
 await mockFleet(page,{mastermind});let posts=0;let submitted="";let matching=false;
 await page.route("**/api/mastermind/migration**",route=>{const req=route.request();if(req.method()==="GET")return route.fulfill({json:posts?{operation:{id:matching?submitted:"a".repeat(32),hostId:"dev",status:matching?"active":"failed",stage:matching?"Destination active":"Older failed operation",updatedAt:new Date().toISOString()},...(matching?{hosted:{hostId:"dev",coordinatorId:"c".repeat(32)}}:{})}: {}});if(req.url().endsWith("/preflight"))return route.fulfill({json:plan()});posts++;submitted=req.postDataJSON().id;return route.abort("failed");});
 await page.goto("/#mastermind");await page.getByRole("button",{name:"Move Mastermind",exact:true}).click();await page.getByRole("button",{name:"Check destination",exact:true}).click();await page.getByRole("dialog").getByRole("checkbox").check();await page.getByRole("button",{name:"Move and start Mastermind",exact:true}).click();
 await expect(page.getByRole("dialog").getByRole("alert")).toContainText("may have completed");await page.reload();
 await expect(page.getByLabel("Mastermind location")).toContainText("move request is unconfirmed");await expect(page.getByRole("button",{name:"Move Mastermind",exact:true})).toBeDisabled();expect(posts).toBe(1);
 matching=true;await expect(page.getByLabel("Mastermind location")).toContainText("Destination active",{timeout:10000});await expect(page.getByLabel("Mastermind location")).not.toContainText("move request is unconfirmed");expect(posts).toBe(1);
});

test("changing destination invalidates a prepared plan",async({page})=>{
 await mockFleet(page,{mastermind,hosts:[local,remote,{...remote,id:"other",name:"Other machine",target:"other@example.test"}]});const preflights:string[]=[];
 await page.route("**/api/mastermind/migration**",route=>{if(route.request().method()==="GET")return route.fulfill({json:{}});const hostId=route.request().postDataJSON().hostId;preflights.push(hostId);return route.fulfill({json:plan(hostId)});});
 await page.goto("/#mastermind");await page.getByRole("button",{name:"Move Mastermind",exact:true}).click();await page.getByRole("button",{name:"Check destination",exact:true}).click();await expect(page.locator(".migration-counts")).toBeVisible();await page.getByLabel("Destination machine").selectOption("other");
 await expect(page.locator(".migration-counts")).toHaveCount(0);await expect(page.getByRole("button",{name:"Move and start Mastermind",exact:true})).toBeDisabled();await page.getByRole("button",{name:"Check destination",exact:true}).click();await expect(page.locator(".migration-counts")).toBeVisible();expect(preflights).toEqual(["dev","other"]);
});

test("a hosted coordinator shows its persistent location without legacy switch-to-local advice",async({page})=>{
 await mockFleet(page,{mastermind:{...mastermind,controllerHostId:"dev",config:{...mastermind.config!,hostId:"dev"}}});
 await page.route("**/api/mastermind/migration",route=>route.fulfill({json:{hosted:{hostId:"dev",coordinatorId:"c".repeat(32)},operation:{id:"a".repeat(32),hostId:"dev",status:"active",stage:"Destination active",updatedAt:new Date().toISOString()}}}));
 await page.goto("/#mastermind");await expect(page.getByLabel("Mastermind location")).toContainText("Hosted on Development");await expect(page.getByRole("button",{name:"Switch to local",exact:true})).toHaveCount(0);await expect(page.getByRole("button",{name:"Move Mastermind",exact:true})).toBeDisabled();await expect(page.getByLabel("Mastermind location")).toContainText("adopting it from a different desktop is not available yet");expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);
});
