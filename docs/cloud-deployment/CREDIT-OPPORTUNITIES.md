# AWS credit opportunities — what is real and what is not

Checked 2026-10-28 against official AWS pages and this account's own read-only
APIs. Nothing was enrolled in, redeemed or accepted.

---

## The headline

**The "$20 per activity" offer is real, but this account cannot have it.**
It belongs to the July-2025 Free Tier plan, which AWS restricts to *new*
customers. The Free Tier FAQ states that "the Free plan and Free Tier credits
are available only to new AWS customers", that existing accounts — or accounts
you have previously had — are **ineligible**, and that an existing account
**cannot be converted** to the Free plan.

This account is pre-existing: it carries three unrelated student projects and
three months of continuous billed usage. It is not a new customer.

---

## What this account's own APIs prove

| Query | Result |
|---|---|
| `ce get-cost-and-usage`, record type, Aug–Oct 2026 | **Credits covered 100 % of usage.** Aug $42.94, Sep $77.84, Oct $8.05 — every dollar offset by a `Credit` line of equal magnitude |
| Out-of-pocket across three months | **$0.00** |
| `freetier get-free-tier-usage` | 5 entries, **all `Always Free`**; **zero `12 Months Free`** |
| `budgets describe-budgets` | **AccessDenied** |
| `organizations describe-organization` | Not in an organisation |
| `support describe-cases` | **SubscriptionRequired** (no paid support plan) |

**Two things follow.** First, the existing credits are live and genuinely
cover EC2, EBS, VPC, data transfer, ECR, RDS and S3 — the exact services this
project needs. Second, there is **no legacy 12-month free tier**, so every
resource prices at full on-demand and is then paid out of credit.

**The balance itself cannot be read.** AWS exposes no API for a credit
balance; Cost Explorer shows credits *applied*, not credits *remaining*. The
$31 figure is your console reading and I could not verify it independently.

Verify it manually at
**Billing and Cost Management → Credits**:
`https://us-east-1.console.aws.amazon.com/billing/home#/credits`
That page shows amount remaining, expiry date, and the services each credit is
restricted to.

---

## Every offer investigated

| Offer | Amount | Eligibility | Required action | Cost to qualify | Restrictions | Stacking | Status |
|---|---|---|---|---|---|---|---|
| **Free Tier sign-up credit** | $100 | New customers only | Create a new account | None | Expires 6 months after account creation | n/a | **UNAVAILABLE** — existing account |
| **Free Tier activity credits** (the "$20" claim) | $20 × 5 = $100 | New customers only, via the *Explore AWS* console widget | Use EC2, RDS, Lambda, Bedrock, Budgets | None | Expires 12 months after account creation; does not unlock over time | With sign-up credit | **UNAVAILABLE** — existing account cannot convert |
| **AWS Educate** | **$0 in account credits** | Students 13+, email only | Register | None | Provides *labs and training only* | n/a | **NO CREDITS** — the official page describes hands-on labs and badges, never credits to your own account |
| **AWS Student Rewards / Builder Center** | Reported $10 at 7 badges, +$20 at 14 | Students 18+ at accredited institutions | Verify student status, earn badges | None | Amounts seen only in third-party write-ups | Unknown | **UNVERIFIED** — plausible, small, worth ten minutes of your own checking |
| **AWS Activate** | $1k–$200k | Startups, not students; typically must not have had prior Activate credits | Apply with a company, often needs a partner referral | None, but needs a real company | Rejections common for accounts with prior credits | Rarely | **NOT ELIGIBLE** — this is a course project, not a startup |
| **Certification / event vouchers** | Varies | Event- and campaign-specific | Attend, complete | Sometimes an exam fee | Usually exam vouchers, **not** account credits | n/a | **OUT OF SCOPE** — vouchers are not spendable on EC2 |

---

## Ranked, realistic ways to increase usable credit

1. **Read the console Credits page and record the expiry date.** The single
   highest-value action, and it costs nothing. If the credits expire before the
   campaign, that changes the schedule rather than the budget.
2. **Check AWS Student Rewards at the Builder Center.** Potentially $10–$30,
   unverified, no cost, ten minutes. Treat any result as a bonus.
3. **Spend less rather than earn more.** The campaign designed in
   `COST-OPTIMIZATION-PLAN.md` costs **$0.88–$3.51**. Against a $31 balance,
   effort spent hunting credits is worth less than effort spent testing.

**Do not** create a second AWS account to claim the new-customer offer. It
would be a different account with different credentials, the existing projects
and credits would not move, and AWS's terms address multiple accounts directly.

---

## Answer to the question as asked

> *Can I realistically get another $20 in AWS credits?*

**No, not from the offer you heard about.** It requires a new AWS account, and
AWS explicitly excludes existing accounts and forbids conversion. The only
unverified possibility is AWS Student Rewards, which reports smaller amounts
and which you would need to check against your own student status.

**This does not matter for the plan.** The verified position is that credits
are currently absorbing 100 % of this account's usage, and the recommended
test campaign costs under $4.

---

*Sources: AWS Free Tier FAQ (`aws.amazon.com/free/free-tier-faqs/`), AWS
Educate (`aws.amazon.com/education/awseducate/`), and the July 2025 AWS Free
Tier announcement. Account facts from `aws ce get-cost-and-usage` and
`aws freetier get-free-tier-usage`, checked 2026-10-28. Cost Explorer calls
cost $0.01 each; three were made during this audit.*
