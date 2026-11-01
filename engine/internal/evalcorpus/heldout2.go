package evalcorpus

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// THE SECOND HELD-OUT CORPUS
// ---------------------------------------------------------------------------
//
// heldout.go was measured once, with generic-v2 frozen at commit ff3de42, and
// its failures were then read. Two general defects were found that way, so a
// generic-v2 fixed for them can no longer be scored honestly on heldout.go.
// This corpus is the replacement: ten more domains, written and committed
// BEFORE those fixes were made, under the same construction rules. The fixed
// comparator is to be measured on it once.

// HeldOut2DomainOrder fixes the reporting order.
var HeldOut2DomainOrder = []string{
	"cab", "invoice", "leave", "billpay", "tickets",
	"backup", "access", "table", "repair", "transfer",
}

var heldOut2 = map[string]domainSet{

	"cab": {
		Activity: "BookCab",
		Shape:    "a pickup, a drop, a time, a cab type",
		Records: []record{
			{ID: "cb-whitefield-airport-0530-sedan", What: "Whitefield to airport, 5:30, sedan", Phrasings: []string{
				"book a sedan cab from Whitefield to the airport at 5:30",
				"I need a sedan at 5.30 am, pick me up in Whitefield and drop me at the airport",
				"cab: Whitefield -> airport, 05:30, sedan",
				"get a sedan to take me from Whitefield to the airport, leaving half past five in the morning",
			}},
			{ID: "cb-whitefield-airport-0530-mini", What: "Whitefield to airport, 5:30, mini", Phrasings: []string{
				"book a mini cab from Whitefield to the airport at 5:30",
				"I need a mini at 5.30 am, pick me up in Whitefield and drop me at the airport",
				"cab: Whitefield -> airport, 05:30, mini",
				"get a mini to take me from Whitefield to the airport, leaving half past five in the morning",
			}},
			{ID: "cb-whitefield-airport-0630-sedan", What: "Whitefield to airport, 6:30, sedan", Phrasings: []string{
				"book a sedan cab from Whitefield to the airport at 6:30",
				"I need a sedan at 6.30 am, pick me up in Whitefield and drop me at the airport",
				"cab: Whitefield -> airport, 06:30, sedan",
				"get a sedan to take me from Whitefield to the airport, leaving half past six in the morning",
			}},
			{ID: "cb-airport-whitefield-0530-sedan", What: "airport to Whitefield, 5:30, sedan", Phrasings: []string{
				"book a sedan cab from the airport to Whitefield at 5:30",
				"I need a sedan at 5.30 am, pick me up at the airport and drop me in Whitefield",
				"cab: airport -> Whitefield, 05:30, sedan",
				"get a sedan to take me from the airport to Whitefield, leaving half past five in the morning",
			}},
			{ID: "cb-jayanagar-station-1900-auto", What: "Jayanagar to the railway station, 19:00, auto", Phrasings: []string{
				"book an auto from Jayanagar to the railway station at 7 pm",
				"I need an auto at 19:00, pick me up in Jayanagar and drop me at the railway station",
				"auto: Jayanagar -> railway station, 19:00",
				"get an autorickshaw to take me from Jayanagar to the station at seven in the evening",
			}},
		},
		Contrasts: []against{
			{A: "cb-whitefield-airport-0530-sedan", B: "cb-whitefield-airport-0530-mini", Reason: "same ride, a different cab type"},
			{A: "cb-whitefield-airport-0530-sedan", B: "cb-whitefield-airport-0630-sedan", Reason: "same ride, an hour later"},
			{A: "cb-whitefield-airport-0530-sedan", B: "cb-airport-whitefield-0530-sedan", Reason: "same places, the direction reversed"},
			{A: "cb-whitefield-airport-0530-mini", B: "cb-whitefield-airport-0630-sedan", Reason: "a different type and a different time"},
			{A: "cb-whitefield-airport-0530-sedan", B: "cb-jayanagar-station-1900-auto", Reason: "unrelated ride", Easy: true},
		},
	},

	"invoice": {
		Activity: "SendInvoice",
		Shape:    "a client, an amount, a due date",
		Records: []record{
			{ID: "iv-orbit-120000-0115", What: "invoice Orbit Labs 1,20,000 INR due 15 January", Phrasings: []string{
				"send Orbit Labs an invoice for 120000 INR due on 15 January",
				"bill Orbit Labs Rs 1,20,000, payment due Jan 15",
				"invoice: Orbit Labs, INR 120000, due 15/01",
				"raise an invoice of one lakh twenty thousand rupees to Orbit Labs, payable by the 15th of January",
			}},
			{ID: "iv-orbit-102000-0115", What: "invoice Orbit Labs 1,02,000 INR due 15 January", Phrasings: []string{
				"send Orbit Labs an invoice for 102000 INR due on 15 January",
				"bill Orbit Labs Rs 1,02,000, payment due Jan 15",
				"invoice: Orbit Labs, INR 102000, due 15/01",
				"raise an invoice of one lakh two thousand rupees to Orbit Labs, payable by the 15th of January",
			}},
			{ID: "iv-orbit-120000-0131", What: "invoice Orbit Labs 1,20,000 INR due 31 January", Phrasings: []string{
				"send Orbit Labs an invoice for 120000 INR due on 31 January",
				"bill Orbit Labs Rs 1,20,000, payment due Jan 31",
				"invoice: Orbit Labs, INR 120000, due 31/01",
				"raise an invoice of one lakh twenty thousand rupees to Orbit Labs, payable by the 31st of January",
			}},
			{ID: "iv-nimbus-120000-0115", What: "invoice Nimbus Retail 1,20,000 INR due 15 January", Phrasings: []string{
				"send Nimbus Retail an invoice for 120000 INR due on 15 January",
				"bill Nimbus Retail Rs 1,20,000, payment due Jan 15",
				"invoice: Nimbus Retail, INR 120000, due 15/01",
				"raise an invoice of one lakh twenty thousand rupees to Nimbus Retail, payable by the 15th of January",
			}},
			{ID: "iv-kite-8800-usd-0301", What: "invoice Kite Studio 8,800 USD due 1 March", Phrasings: []string{
				"send Kite Studio an invoice for 8800 USD due on 1 March",
				"bill Kite Studio $8,800, payment due Mar 1",
				"invoice: Kite Studio, USD 8800, due 01/03",
				"raise an invoice of eight thousand eight hundred dollars to Kite Studio, payable by the 1st of March",
			}},
		},
		Contrasts: []against{
			{A: "iv-orbit-120000-0115", B: "iv-orbit-102000-0115", Reason: "same client and date, a different amount"},
			{A: "iv-orbit-120000-0115", B: "iv-orbit-120000-0131", Reason: "same client and amount, a later due date"},
			{A: "iv-orbit-120000-0115", B: "iv-nimbus-120000-0115", Reason: "same amount and date, a different client"},
			{A: "iv-orbit-102000-0115", B: "iv-orbit-120000-0131", Reason: "a different amount and a different date"},
			{A: "iv-orbit-120000-0115", B: "iv-kite-8800-usd-0301", Reason: "unrelated invoice", Easy: true},
		},
	},

	"leave": {
		Activity: "ApplyLeave",
		Shape:    "a start date, a number of days, a kind of leave",
		Records: []record{
			{ID: "lv-0602-3-sick", What: "sick leave from 2 June, 3 days", Phrasings: []string{
				"apply for 3 days of sick leave starting 2 June",
				"put in a sick leave request from June 2nd for three days",
				"sick leave, 02/06, 3 days",
				"I am unwell, please file three days' sick leave beginning the 2nd of June",
			}},
			{ID: "lv-0602-3-casual", What: "casual leave from 2 June, 3 days", Phrasings: []string{
				"apply for 3 days of casual leave starting 2 June",
				"put in a casual leave request from June 2nd for three days",
				"casual leave, 02/06, 3 days",
				"I need a break, please file three days' casual leave beginning the 2nd of June",
			}},
			{ID: "lv-0602-5-sick", What: "sick leave from 2 June, 5 days", Phrasings: []string{
				"apply for 5 days of sick leave starting 2 June",
				"put in a sick leave request from June 2nd for five days",
				"sick leave, 02/06, 5 days",
				"I am unwell, please file five days' sick leave beginning the 2nd of June",
			}},
			{ID: "lv-0609-3-sick", What: "sick leave from 9 June, 3 days", Phrasings: []string{
				"apply for 3 days of sick leave starting 9 June",
				"put in a sick leave request from June 9th for three days",
				"sick leave, 09/06, 3 days",
				"I am unwell, please file three days' sick leave beginning the 9th of June",
			}},
			{ID: "lv-1224-7-annual", What: "annual leave from 24 December, 7 days", Phrasings: []string{
				"apply for 7 days of annual leave starting 24 December",
				"put in an annual leave request from December 24th for seven days",
				"annual leave, 24/12, 7 days",
				"holiday time: please file seven days' annual leave beginning Christmas Eve",
			}},
		},
		Contrasts: []against{
			{A: "lv-0602-3-sick", B: "lv-0602-3-casual", Reason: "same dates, a different kind of leave"},
			{A: "lv-0602-3-sick", B: "lv-0602-5-sick", Reason: "same start and kind, two more days"},
			{A: "lv-0602-3-sick", B: "lv-0609-3-sick", Reason: "same length and kind, a week later"},
			{A: "lv-0602-3-casual", B: "lv-0602-5-sick", Reason: "a different kind and a different length"},
			{A: "lv-0602-3-sick", B: "lv-1224-7-annual", Reason: "unrelated leave", Easy: true},
		},
	},

	"billpay": {
		Activity: "PayBill",
		Shape:    "a biller, an account number, an amount",
		Records: []record{
			{ID: "bp-bescom-7731-2140", What: "pay BESCOM account 7731, 2,140 INR", Phrasings: []string{
				"pay the BESCOM electricity bill for account 7731, Rs 2140",
				"settle 2,140 rupees on BESCOM account no. 7731",
				"BESCOM 7731, pay 2140 INR",
				"clear my electricity bill with BESCOM, consumer number 7731, two thousand one hundred and forty rupees",
			}},
			{ID: "bp-bescom-7731-1240", What: "pay BESCOM account 7731, 1,240 INR", Phrasings: []string{
				"pay the BESCOM electricity bill for account 7731, Rs 1240",
				"settle 1,240 rupees on BESCOM account no. 7731",
				"BESCOM 7731, pay 1240 INR",
				"clear my electricity bill with BESCOM, consumer number 7731, one thousand two hundred and forty rupees",
			}},
			{ID: "bp-bescom-7713-2140", What: "pay BESCOM account 7713, 2,140 INR", Phrasings: []string{
				"pay the BESCOM electricity bill for account 7713, Rs 2140",
				"settle 2,140 rupees on BESCOM account no. 7713",
				"BESCOM 7713, pay 2140 INR",
				"clear my electricity bill with BESCOM, consumer number 7713, two thousand one hundred and forty rupees",
			}},
			{ID: "bp-bwssb-7731-2140", What: "pay BWSSB account 7731, 2,140 INR", Phrasings: []string{
				"pay the BWSSB water bill for account 7731, Rs 2140",
				"settle 2,140 rupees on BWSSB account no. 7731",
				"BWSSB 7731, pay 2140 INR",
				"clear my water bill with BWSSB, consumer number 7731, two thousand one hundred and forty rupees",
			}},
			{ID: "bp-airtel-9900-599", What: "pay Airtel mobile 9900, 599 INR", Phrasings: []string{
				"pay the Airtel mobile bill for account 9900, Rs 599",
				"settle 599 rupees on Airtel account no. 9900",
				"Airtel 9900, pay 599 INR",
				"clear my phone bill with Airtel, number ending 9900, five hundred and ninety-nine rupees",
			}},
		},
		Contrasts: []against{
			{A: "bp-bescom-7731-2140", B: "bp-bescom-7731-1240", Reason: "same account, two digits of the amount swapped"},
			{A: "bp-bescom-7731-2140", B: "bp-bescom-7713-2140", Reason: "same biller and amount, two digits of the account transposed"},
			{A: "bp-bescom-7731-2140", B: "bp-bwssb-7731-2140", Reason: "same account and amount, a different biller"},
			{A: "bp-bescom-7731-1240", B: "bp-bescom-7713-2140", Reason: "a different account and a different amount"},
			{A: "bp-bescom-7731-2140", B: "bp-airtel-9900-599", Reason: "unrelated bill", Easy: true},
		},
	},

	"tickets": {
		Activity: "BuyEventTickets",
		Shape:    "an event, a date, a count, a seating tier",
		Records: []record{
			{ID: "tk-coldplay-0118-2-gold", What: "Coldplay 18 January, 2 tickets, gold", Phrasings: []string{
				"buy 2 gold tickets for Coldplay on 18 January",
				"get two Coldplay tickets in the gold section for Jan 18",
				"Coldplay, 18/01, 2 x gold",
				"I want a pair of gold-tier seats for the Coldplay show on the 18th of January",
			}},
			{ID: "tk-coldplay-0118-2-silver", What: "Coldplay 18 January, 2 tickets, silver", Phrasings: []string{
				"buy 2 silver tickets for Coldplay on 18 January",
				"get two Coldplay tickets in the silver section for Jan 18",
				"Coldplay, 18/01, 2 x silver",
				"I want a pair of silver-tier seats for the Coldplay show on the 18th of January",
			}},
			{ID: "tk-coldplay-0119-2-gold", What: "Coldplay 19 January, 2 tickets, gold", Phrasings: []string{
				"buy 2 gold tickets for Coldplay on 19 January",
				"get two Coldplay tickets in the gold section for Jan 19",
				"Coldplay, 19/01, 2 x gold",
				"I want a pair of gold-tier seats for the Coldplay show on the 19th of January",
			}},
			{ID: "tk-coldplay-0118-4-gold", What: "Coldplay 18 January, 4 tickets, gold", Phrasings: []string{
				"buy 4 gold tickets for Coldplay on 18 January",
				"get four Coldplay tickets in the gold section for Jan 18",
				"Coldplay, 18/01, 4 x gold",
				"I want four gold-tier seats for the Coldplay show on the 18th of January",
			}},
			{ID: "tk-ipl-0412-3-north", What: "IPL match 12 April, 3 tickets, north stand", Phrasings: []string{
				"buy 3 north stand tickets for the IPL match on 12 April",
				"get three IPL tickets in the north stand for Apr 12",
				"IPL, 12/04, 3 x north stand",
				"I want three seats in the north stand for the IPL game on the 12th of April",
			}},
		},
		Contrasts: []against{
			{A: "tk-coldplay-0118-2-gold", B: "tk-coldplay-0118-2-silver", Reason: "same show and count, a different tier"},
			{A: "tk-coldplay-0118-2-gold", B: "tk-coldplay-0119-2-gold", Reason: "same tier and count, the next night"},
			{A: "tk-coldplay-0118-2-gold", B: "tk-coldplay-0118-4-gold", Reason: "same show and tier, twice the count"},
			{A: "tk-coldplay-0118-2-silver", B: "tk-coldplay-0119-2-gold", Reason: "a different tier and a different night"},
			{A: "tk-coldplay-0118-2-gold", B: "tk-ipl-0412-3-north", Reason: "unrelated event", Easy: true},
		},
	},

	"backup": {
		Activity: "RunBackup",
		Shape:    "a system, a destination bucket, a retention period",
		Records: []record{
			{ID: "bk-orders-db-s3archive-30d", What: "back up orders-db to s3://archive, keep 30 days", Phrasings: []string{
				"back up orders-db to s3://archive and keep it for 30 days",
				"take a backup of the orders-db database into s3://archive, 30-day retention",
				"backup orders-db -> s3://archive, retain 30d",
				"copy orders-db to the s3://archive bucket and hold the copy for a month of 30 days",
			}},
			{ID: "bk-orders-db-s3archive-90d", What: "back up orders-db to s3://archive, keep 90 days", Phrasings: []string{
				"back up orders-db to s3://archive and keep it for 90 days",
				"take a backup of the orders-db database into s3://archive, 90-day retention",
				"backup orders-db -> s3://archive, retain 90d",
				"copy orders-db to the s3://archive bucket and hold the copy for 90 days",
			}},
			{ID: "bk-orders-db-s3cold-30d", What: "back up orders-db to s3://cold, keep 30 days", Phrasings: []string{
				"back up orders-db to s3://cold and keep it for 30 days",
				"take a backup of the orders-db database into s3://cold, 30-day retention",
				"backup orders-db -> s3://cold, retain 30d",
				"copy orders-db to the s3://cold bucket and hold the copy for a month of 30 days",
			}},
			{ID: "bk-users-db-s3archive-30d", What: "back up users-db to s3://archive, keep 30 days", Phrasings: []string{
				"back up users-db to s3://archive and keep it for 30 days",
				"take a backup of the users-db database into s3://archive, 30-day retention",
				"backup users-db -> s3://archive, retain 30d",
				"copy users-db to the s3://archive bucket and hold the copy for a month of 30 days",
			}},
			{ID: "bk-logs-gcs-7d", What: "back up app-logs to gs://logs, keep 7 days", Phrasings: []string{
				"back up app-logs to gs://logs and keep it for 7 days",
				"take a backup of the app-logs volume into gs://logs, 7-day retention",
				"backup app-logs -> gs://logs, retain 7d",
				"copy app-logs to the gs://logs bucket and hold the copy for a week",
			}},
		},
		Contrasts: []against{
			{A: "bk-orders-db-s3archive-30d", B: "bk-orders-db-s3archive-90d", Reason: "same backup, a longer retention"},
			{A: "bk-orders-db-s3archive-30d", B: "bk-orders-db-s3cold-30d", Reason: "same system and retention, a different bucket"},
			{A: "bk-orders-db-s3archive-30d", B: "bk-users-db-s3archive-30d", Reason: "same bucket and retention, a different system"},
			{A: "bk-orders-db-s3archive-90d", B: "bk-orders-db-s3cold-30d", Reason: "a different bucket and a different retention"},
			{A: "bk-orders-db-s3archive-30d", B: "bk-logs-gcs-7d", Reason: "unrelated backup", Easy: true},
		},
	},

	"access": {
		Activity: "GrantAccess",
		Shape:    "a user, a role, a project",
		Records: []record{
			{ID: "ac-kiran-viewer-atlas", What: "grant kiran@acme.test viewer on project atlas", Phrasings: []string{
				"grant kiran@acme.test the viewer role on project atlas",
				"give kiran@acme.test read-only viewer access to the atlas project",
				"atlas: kiran@acme.test = viewer",
				"add Kiran (kiran@acme.test) to atlas as a viewer",
			}},
			{ID: "ac-kiran-admin-atlas", What: "grant kiran@acme.test admin on project atlas", Phrasings: []string{
				"grant kiran@acme.test the admin role on project atlas",
				"give kiran@acme.test full admin access to the atlas project",
				"atlas: kiran@acme.test = admin",
				"add Kiran (kiran@acme.test) to atlas as an admin",
			}},
			{ID: "ac-kiran-viewer-boreas", What: "grant kiran@acme.test viewer on project boreas", Phrasings: []string{
				"grant kiran@acme.test the viewer role on project boreas",
				"give kiran@acme.test read-only viewer access to the boreas project",
				"boreas: kiran@acme.test = viewer",
				"add Kiran (kiran@acme.test) to boreas as a viewer",
			}},
			{ID: "ac-kavya-viewer-atlas", What: "grant kavya@acme.test viewer on project atlas", Phrasings: []string{
				"grant kavya@acme.test the viewer role on project atlas",
				"give kavya@acme.test read-only viewer access to the atlas project",
				"atlas: kavya@acme.test = viewer",
				"add Kavya (kavya@acme.test) to atlas as a viewer",
			}},
			{ID: "ac-ci-bot-deployer-zephyr", What: "grant ci-bot@acme.test deployer on project zephyr", Phrasings: []string{
				"grant ci-bot@acme.test the deployer role on project zephyr",
				"give ci-bot@acme.test deployer access to the zephyr project",
				"zephyr: ci-bot@acme.test = deployer",
				"add the CI bot (ci-bot@acme.test) to zephyr as a deployer",
			}},
		},
		Contrasts: []against{
			{A: "ac-kiran-viewer-atlas", B: "ac-kiran-admin-atlas", Reason: "same user and project, a different role"},
			{A: "ac-kiran-viewer-atlas", B: "ac-kiran-viewer-boreas", Reason: "same user and role, a different project"},
			{A: "ac-kiran-viewer-atlas", B: "ac-kavya-viewer-atlas", Reason: "same role and project, a different user with a similar name"},
			{A: "ac-kiran-admin-atlas", B: "ac-kiran-viewer-boreas", Reason: "a different role and a different project"},
			{A: "ac-kiran-viewer-atlas", B: "ac-ci-bot-deployer-zephyr", Reason: "unrelated grant", Easy: true},
		},
	},

	"table": {
		Activity: "ReserveTable",
		Shape:    "a restaurant, a day, a time, a party size",
		Records: []record{
			{ID: "tb-toit-sat-2000-4", What: "Toit, Saturday 8 pm, 4 people", Phrasings: []string{
				"reserve a table for 4 at Toit on Saturday at 8 pm",
				"book Toit for four people this Saturday, 20:00",
				"Toit, Sat 8pm, party of 4",
				"get us a table at Toit for four on Saturday evening at eight",
			}},
			{ID: "tb-toit-sat-2100-4", What: "Toit, Saturday 9 pm, 4 people", Phrasings: []string{
				"reserve a table for 4 at Toit on Saturday at 9 pm",
				"book Toit for four people this Saturday, 21:00",
				"Toit, Sat 9pm, party of 4",
				"get us a table at Toit for four on Saturday evening at nine",
			}},
			{ID: "tb-toit-sun-2000-4", What: "Toit, Sunday 8 pm, 4 people", Phrasings: []string{
				"reserve a table for 4 at Toit on Sunday at 8 pm",
				"book Toit for four people this Sunday, 20:00",
				"Toit, Sun 8pm, party of 4",
				"get us a table at Toit for four on Sunday evening at eight",
			}},
			{ID: "tb-toit-sat-2000-6", What: "Toit, Saturday 8 pm, 6 people", Phrasings: []string{
				"reserve a table for 6 at Toit on Saturday at 8 pm",
				"book Toit for six people this Saturday, 20:00",
				"Toit, Sat 8pm, party of 6",
				"get us a table at Toit for six on Saturday evening at eight",
			}},
			{ID: "tb-mtr-sun-0800-2", What: "MTR, Sunday 8 am, 2 people", Phrasings: []string{
				"reserve a table for 2 at MTR on Sunday at 8 am",
				"book MTR for two people this Sunday, 08:00",
				"MTR, Sun 8am, party of 2",
				"get us a table at MTR for two on Sunday morning at eight",
			}},
		},
		Contrasts: []against{
			{A: "tb-toit-sat-2000-4", B: "tb-toit-sat-2100-4", Reason: "same table, an hour later"},
			{A: "tb-toit-sat-2000-4", B: "tb-toit-sun-2000-4", Reason: "same time and size, the next day"},
			{A: "tb-toit-sat-2000-4", B: "tb-toit-sat-2000-6", Reason: "same slot, a larger party"},
			{A: "tb-toit-sat-2100-4", B: "tb-toit-sun-2000-4", Reason: "a different day and a different time"},
			{A: "tb-toit-sat-2000-4", B: "tb-mtr-sun-0800-2", Reason: "unrelated booking", Easy: true},
		},
	},

	"repair": {
		Activity: "ScheduleRepair",
		Shape:    "an appliance, an address, a day, a slot",
		Records: []record{
			{ID: "rp-washer-flat304-mon-am", What: "washing machine, flat 304, Monday morning", Phrasings: []string{
				"schedule a washing machine repair at flat 304 on Monday morning",
				"send a technician to fix the washing machine in flat 304, Monday AM",
				"repair: washing machine, flat 304, Mon morning",
				"our washer in flat 304 is broken, can someone come Monday before noon",
			}},
			{ID: "rp-fridge-flat304-mon-am", What: "refrigerator, flat 304, Monday morning", Phrasings: []string{
				"schedule a refrigerator repair at flat 304 on Monday morning",
				"send a technician to fix the refrigerator in flat 304, Monday AM",
				"repair: refrigerator, flat 304, Mon morning",
				"our fridge in flat 304 is broken, can someone come Monday before noon",
			}},
			{ID: "rp-washer-flat340-mon-am", What: "washing machine, flat 340, Monday morning", Phrasings: []string{
				"schedule a washing machine repair at flat 340 on Monday morning",
				"send a technician to fix the washing machine in flat 340, Monday AM",
				"repair: washing machine, flat 340, Mon morning",
				"our washer in flat 340 is broken, can someone come Monday before noon",
			}},
			{ID: "rp-washer-flat304-mon-pm", What: "washing machine, flat 304, Monday afternoon", Phrasings: []string{
				"schedule a washing machine repair at flat 304 on Monday afternoon",
				"send a technician to fix the washing machine in flat 304, Monday PM",
				"repair: washing machine, flat 304, Mon afternoon",
				"our washer in flat 304 is broken, can someone come Monday after lunch",
			}},
			{ID: "rp-ac-office2-thu-pm", What: "air conditioner, office 2, Thursday afternoon", Phrasings: []string{
				"schedule an air conditioner repair at office 2 on Thursday afternoon",
				"send a technician to fix the air conditioner in office 2, Thursday PM",
				"repair: air conditioner, office 2, Thu afternoon",
				"the AC in office 2 is broken, can someone come Thursday after lunch",
			}},
		},
		Contrasts: []against{
			{A: "rp-washer-flat304-mon-am", B: "rp-fridge-flat304-mon-am", Reason: "same visit, a different appliance"},
			{A: "rp-washer-flat304-mon-am", B: "rp-washer-flat340-mon-am", Reason: "same visit, digits of the flat transposed"},
			{A: "rp-washer-flat304-mon-am", B: "rp-washer-flat304-mon-pm", Reason: "same visit, the afternoon instead"},
			{A: "rp-fridge-flat304-mon-am", B: "rp-washer-flat304-mon-pm", Reason: "a different appliance and a different slot"},
			{A: "rp-washer-flat304-mon-am", B: "rp-ac-office2-thu-pm", Reason: "unrelated repair", Easy: true},
		},
	},

	"transfer": {
		Activity: "TransferFunds",
		Shape:    "a source account, a destination account, an amount",
		Records: []record{
			{ID: "tf-savings-current-25000", What: "move 25,000 INR from savings to current", Phrasings: []string{
				"transfer 25000 INR from savings to current",
				"move Rs 25,000 out of my savings account into the current account",
				"savings -> current, 25000 rupees",
				"shift twenty-five thousand rupees from the savings account over to current",
			}},
			{ID: "tf-current-savings-25000", What: "move 25,000 INR from current to savings", Phrasings: []string{
				"transfer 25000 INR from current to savings",
				"move Rs 25,000 out of my current account into the savings account",
				"current -> savings, 25000 rupees",
				"shift twenty-five thousand rupees from the current account over to savings",
			}},
			{ID: "tf-savings-current-52000", What: "move 52,000 INR from savings to current", Phrasings: []string{
				"transfer 52000 INR from savings to current",
				"move Rs 52,000 out of my savings account into the current account",
				"savings -> current, 52000 rupees",
				"shift fifty-two thousand rupees from the savings account over to current",
			}},
			{ID: "tf-savings-fd-25000", What: "move 25,000 INR from savings to the fixed deposit", Phrasings: []string{
				"transfer 25000 INR from savings to fixed deposit",
				"move Rs 25,000 out of my savings account into the fixed deposit account",
				"savings -> fixed deposit, 25000 rupees",
				"shift twenty-five thousand rupees from the savings account over to the fixed deposit",
			}},
			{ID: "tf-wallet-savings-300", What: "move 300 INR from the wallet to savings", Phrasings: []string{
				"transfer 300 INR from wallet to savings",
				"move Rs 300 out of my wallet into the savings account",
				"wallet -> savings, 300 rupees",
				"shift three hundred rupees from the wallet over to savings",
			}},
		},
		Contrasts: []against{
			{A: "tf-savings-current-25000", B: "tf-current-savings-25000", Reason: "same accounts and amount, the direction reversed"},
			{A: "tf-savings-current-25000", B: "tf-savings-current-52000", Reason: "same transfer, digits of the amount swapped"},
			{A: "tf-savings-current-25000", B: "tf-savings-fd-25000", Reason: "same source and amount, a different destination"},
			{A: "tf-current-savings-25000", B: "tf-savings-current-52000", Reason: "a different direction and a different amount"},
			{A: "tf-savings-current-25000", B: "tf-wallet-savings-300", Reason: "unrelated transfer", Easy: true},
		},
	},
}

// HeldOut2 builds the second held-out corpus. Pair ids start "h2-".
func HeldOut2() ([]Pair, error) {
	byDomain := make(map[string][]Pair, len(HeldOut2DomainOrder))
	for _, name := range HeldOut2DomainOrder {
		set, ok := heldOut2[name]
		if !ok {
			return nil, fmt.Errorf("evalcorpus: HeldOut2DomainOrder names %q but there is no such domain", name)
		}
		pairs, err := buildDomain(name, set)
		if err != nil {
			return nil, err
		}
		for i := range pairs {
			pairs[i].ID = "h2-" + strings.TrimPrefix(pairs[i].ID, "md-")
		}
		byDomain[name] = pairs
	}
	var out []Pair
	for i := 0; ; i++ {
		placed := false
		for _, name := range HeldOut2DomainOrder {
			if i < len(byDomain[name]) {
				out = append(out, byDomain[name][i])
				placed = true
			}
		}
		if !placed {
			break
		}
	}
	if err := validate(out); err != nil {
		return nil, err
	}
	return out, nil
}

// HeldOut2Activity reports the activity type a second-held-out domain uses.
func HeldOut2Activity(domain string) string { return heldOut2[domain].Activity }
